"""GottZ/ctx#566: every REST call of the plugin ends within its bound — status line, headers and body.

urllib applies a timeout to each socket operation (the connect, every read), not to the request: a
server or a proxy that sends its answer a byte at a time, each within the timeout, held a call for as
long as it kept sending (the recall's query ran 28.6 s against Hermes' 8 s wait for the prefetch). The
doubles here are real sockets on 127.0.0.1 that write their answers as scripted: all at once, a byte at
a time, with a pause, or without end. A double that trickles completes its answer after
``TRICKLE_S`` (a valid answer: leading whitespace is valid JSON, as ctx's own keepalive sends it), so a
client without the bound ends there instead of hanging the suite.

Timing: a test that checks "ends before X" takes X = ``SLACK`` times the duration it expects, so load on
the host does not turn it red; what a call must not do (wait for the trickle) is longer than that.
"""

from __future__ import annotations

import datetime
import ipaddress
import json
import select
import socket
import ssl
import threading
import time
import urllib.request
from collections.abc import Callable, Iterator
from typing import Any, ClassVar

import pytest
from conftest import FakeClient

import ctx_plugin.client as client_mod
import ctx_plugin.provider as provider_mod
from ctx_plugin import checkpoint as _cp
from ctx_plugin.client import CtxApiError, CtxClient
from ctx_plugin.config import SESSION_DIGEST_TYPE

KEY = "test-key-566-not-real"
TIMEOUT_S = 0.5  # the client's bound in the client-level tests
SLACK = 5  # "ends before X": X = SLACK x the expected duration
TRICKLE_S = 6.0  # how long a trickling double sends before its answer completes
EVERY_S = 0.05  # one byte per EVERY_S while it trickles
MIB = 1 << 20

Step = bytes | float
Plan = Callable[[str, str, Any], Iterator[Step]]

# -- the doubles ---------------------------------------------------------------------------------

_REASONS = {200: "OK", 302: "Found", 400: "Bad Request", 500: "Internal Server Error"}


def _head(status: int = 200, length: int | None = None, *, extra: tuple[str, ...] = ()) -> bytes:
    lines = [f"HTTP/1.1 {status} {_REASONS[status]}", "Content-Type: application/json", "Connection: close", *extra]
    if length is not None:
        lines.append(f"Content-Length: {length}")
    return ("\r\n".join(lines) + "\r\n\r\n").encode("latin-1")


def answer(obj: Any, status: int = 200) -> Iterator[Step]:
    body = json.dumps(obj).encode()
    yield _head(status, len(body)) + body


def trickled_body(obj: Any, status: int = 200, seconds: float = TRICKLE_S) -> Iterator[Step]:
    """The head at once, then the body a byte per ``EVERY_S`` for ``seconds``, then the rest."""
    n = int(seconds / EVERY_S)
    body = b" " * n + json.dumps(obj).encode()
    yield _head(status, len(body))
    for i in range(n):
        yield body[i : i + 1]
        yield EVERY_S
    yield body[n:]


def trickled_chunks(obj: Any, seconds: float = TRICKLE_S) -> Iterator[Step]:
    """``Transfer-Encoding: chunked``, one one-byte chunk per ``EVERY_S``."""
    yield _head(200, extra=("Transfer-Encoding: chunked",))
    for _ in range(int(seconds / EVERY_S)):
        yield b"1\r\n \r\n"
        yield EVERY_S
    body = json.dumps(obj).encode()
    yield b"%x\r\n" % len(body) + body + b"\r\n0\r\n\r\n"


def trickled_head(
    obj: Any, status: int = 200, seconds: float = TRICKLE_S, extra: tuple[str, ...] = ()
) -> Iterator[Step]:
    """Status line and headers a byte per ``EVERY_S`` for ``seconds``, then the rest at once."""
    n = int(seconds / EVERY_S)
    body = json.dumps(obj).encode()
    raw = _head(status, len(body), extra=(*extra, "X-Pad: " + "p" * n)) + body
    for i in range(n):
        yield raw[i : i + 1]
        yield EVERY_S
    yield raw[n:]


def silent_after_head(obj: Any, seconds: float = TRICKLE_S) -> Iterator[Step]:
    body = json.dumps(obj).encode()
    yield _head(200, len(body))
    yield seconds
    yield body


def late(obj: Any, delay: float, status: int = 200) -> Iterator[Step]:
    yield delay
    yield from answer(obj, status)


def endless(total: int, *, length: int | None = None, chunked: bool = False) -> Iterator[Step]:
    """Data as fast as the client takes it, ``total`` bytes, never a complete JSON document."""
    yield _head(200, length, extra=("Transfer-Encoding: chunked",) if chunked else ())
    piece = b"x" * 65536
    for _ in range(total // len(piece)):
        yield b"10000\r\n" + piece + b"\r\n" if chunked else piece


def _read_request(conn: socket.socket) -> tuple[str, str, Any] | None:
    data = b""
    while b"\r\n\r\n" not in data:
        chunk = conn.recv(65536)
        if not chunk:
            return None
        data += chunk
    head, _, body = data.partition(b"\r\n\r\n")
    lines = head.decode("latin-1").split("\r\n")
    method, path, _ = lines[0].split(" ", 2)
    length = 0
    for line in lines[1:]:
        name, _, value = line.partition(":")
        if name.strip().lower() == "content-length":
            length = int(value)
    while len(body) < length:
        chunk = conn.recv(65536)
        if not chunk:
            break
        body += chunk
    return method, path, json.loads(body) if body else None


def _peer_closed_within(conn: socket.socket, seconds: float) -> bool:
    """Wait ``seconds``; True as soon as the client closed the connection."""
    end = time.monotonic() + seconds
    while (left := end - time.monotonic()) > 0:
        if select.select([conn], [], [], left)[0]:
            try:
                if not conn.recv(4096):
                    return True
            except OSError:
                return True
    return False


class Wire:
    """A REST double on a real socket: each request is answered by ``plan(method, path, body)``,
    whose steps are bytes to send or seconds to wait. Records the requests, the bytes it sent per
    connection and when the client closed a connection before the answer was complete. ``tls``: the
    server's TLS context; ``handshake_delay``: seconds it waits after the accept before its handshake."""

    def __init__(self, plan: Plan, tls: ssl.SSLContext | None = None, handshake_delay: float = 0.0) -> None:
        self.plan = plan
        self.tls = tls
        self.handshake_delay = handshake_delay
        self.requests: list[tuple[str, str, Any]] = []
        self.sent: list[int] = []
        self.closed_by_client: list[float] = []
        self._stop = threading.Event()
        self._threads: list[threading.Thread] = []
        self._listener = socket.create_server(("127.0.0.1", 0))
        self._listener.settimeout(0.1)
        self.url = f"http://127.0.0.1:{self._listener.getsockname()[1]}"
        accepting = threading.Thread(target=self._accept, daemon=True)
        accepting.start()
        self._threads.append(accepting)

    def _accept(self) -> None:
        while not self._stop.is_set():
            try:
                conn, _ = self._listener.accept()
            except TimeoutError:
                continue
            except OSError:
                return
            serving = threading.Thread(target=self._serve, args=(conn,), daemon=True)
            serving.start()
            self._threads.append(serving)

    def _serve(self, conn: socket.socket) -> None:
        sent = 0
        conn.settimeout(10.0)
        if self.tls is not None:
            try:
                if self._stop.wait(self.handshake_delay):  # waits without reading the client's hello
                    conn.close()
                    return
                conn = self.tls.wrap_socket(conn, server_side=True)
            except OSError:
                conn.close()
                return
        with conn:
            request = _read_request(conn)
            if request is None:
                return
            self.requests.append(request)
            try:
                for step in self.plan(*request):
                    if self._stop.is_set():
                        break
                    if isinstance(step, bytes):
                        conn.sendall(step)
                        sent += len(step)
                    elif _peer_closed_within(conn, step):
                        self.closed_by_client.append(time.monotonic())
                        break
            except OSError:
                self.closed_by_client.append(time.monotonic())
            finally:
                self.sent.append(sent)

    def paths(self) -> list[str]:
        return [path for _, path, _ in self.requests]

    def close(self) -> None:
        self._stop.set()
        self._listener.close()
        for thread in self._threads:
            thread.join(10.0)


@pytest.fixture
def wire():
    made: list[Wire] = []

    def make(plan: Plan, **tls: Any) -> Wire:
        made.append(Wire(plan, **tls))
        return made[-1]

    yield make
    for w in made:
        w.close()


@pytest.fixture
def full_backlog():
    """Listeners whose accept queue is full: a connect to them waits until its own timeout (the
    kernel drops the SYN)."""
    opened: list[socket.socket] = []

    def make() -> tuple[str, int]:
        listener = socket.create_server(("127.0.0.1", 0), backlog=0)
        filler = socket.create_connection(listener.getsockname(), timeout=1.0)
        opened.extend((listener, filler))
        return listener.getsockname()

    yield make
    for sock in opened:
        sock.close()


def _timed(fn: Callable[[], Any]) -> tuple[Any, BaseException | None, float]:
    start = time.monotonic()
    try:
        return fn(), None, time.monotonic() - start
    except Exception as exc:
        return None, exc, time.monotonic() - start


def _wait_for(predicate: Callable[[], bool], seconds: float) -> bool:
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        if predicate():
            return True
        time.sleep(0.01)
    return predicate()


SEARCH_ANSWER = {"success": True, "results": [{"id": "b1", "title": "one", "category": "reference"}], "count": 1}

# -- the client: one bound for the whole exchange --------------------------------------------------


@pytest.mark.parametrize(
    "plan",
    [
        lambda: trickled_body(SEARCH_ANSWER),
        lambda: trickled_chunks(SEARCH_ANSWER),
        lambda: trickled_head(SEARCH_ANSWER),
        lambda: trickled_head({"success": False}, status=302, extra=("Location: http://127.0.0.1:9/elsewhere",)),
        lambda: silent_after_head(SEARCH_ANSWER),
    ],
    ids=["body_trickles", "chunks_trickle", "head_trickles", "redirect_head_trickles", "silent_after_head"],
)
def test_a_call_ends_within_its_timeout_whatever_the_answer_trickles(wire, plan):
    """Before, only the silent server ended at the timeout; a trickling one held the call until its
    answer was complete. The error is the one a server that stops sending gives (status and code
    empty, ``connection error: timed out``), so every caller reads it as before."""
    w = wire(lambda *_: plan())
    client = CtxClient(w.url, KEY, timeout=TIMEOUT_S)
    data, error, took = _timed(lambda: client.search("q"))
    assert isinstance(error, CtxApiError), (data, took)
    assert TIMEOUT_S * 0.9 <= took < SLACK * TIMEOUT_S, took
    assert (str(error), error.status, error.code, error.permanent) == ("connection error: timed out", None, "", False)


def test_the_connection_is_closed_when_the_call_gives_up(wire):
    """A call given up on closes its socket: the double sees the close at once and stops sending (no
    connection, and no thread, is left behind holding the server)."""
    w = wire(lambda *_: trickled_body(SEARCH_ANSWER))
    client = CtxClient(w.url, KEY, timeout=TIMEOUT_S)
    _, error, _ = _timed(lambda: client.search("q"))
    returned = time.monotonic()
    assert isinstance(error, CtxApiError)
    assert _wait_for(lambda: bool(w.closed_by_client), SLACK * TIMEOUT_S), "the double never saw the close"
    assert w.closed_by_client[0] - returned < SLACK * TIMEOUT_S


def test_an_error_answer_whose_body_trickles_ends_within_the_timeout(wire):
    """The text of an error answer is read within the same bound; when it does not arrive in time,
    the error keeps its status and names the reason instead."""
    w = wire(lambda *_: trickled_body({"success": False, "error": "upstream failed"}, status=500))
    client = CtxClient(w.url, KEY, timeout=TIMEOUT_S)
    _, error, took = _timed(lambda: client.search("q"))
    assert isinstance(error, CtxApiError)
    assert took < SLACK * TIMEOUT_S, took
    assert error.status == 500 and str(error).startswith("HTTP 500"), str(error)


def test_a_connect_ends_within_the_timeout_over_every_address_of_the_host(full_backlog, monkeypatch):
    """A host name with several addresses, none of which answers the connect: each address used to
    get the whole timeout (six addresses, six timeouts); the call ends within one."""
    addresses = [full_backlog() for _ in range(6)]
    real = socket.getaddrinfo

    def getaddrinfo(host, port, *args, **kwargs):
        if host == "six.addresses.test":
            return [(socket.AF_INET, socket.SOCK_STREAM, 6, "", a) for a in addresses]
        return real(host, port, *args, **kwargs)

    monkeypatch.setattr(socket, "getaddrinfo", getaddrinfo)
    client = CtxClient("http://six.addresses.test:8080", KEY, timeout=TIMEOUT_S)
    _, error, took = _timed(lambda: client.search("q"))
    assert isinstance(error, CtxApiError) and error.status is None and error.code == ""
    assert "timed out" in str(error), str(error)
    assert TIMEOUT_S * 0.9 <= took < SLACK * TIMEOUT_S, took


def test_a_connect_to_one_silent_address_ends_at_the_timeout(full_backlog):
    host, port = full_backlog()
    client = CtxClient(f"http://{host}:{port}", KEY, timeout=TIMEOUT_S)
    _, error, took = _timed(lambda: client.search("q"))
    assert isinstance(error, CtxApiError) and "timed out" in str(error), error
    assert TIMEOUT_S * 0.9 <= took < SLACK * TIMEOUT_S, took


def test_a_redirect_whose_body_trickles_is_refused_at_once(wire):
    w = wire(lambda *_: trickled_body({"moved": True}, status=302))
    client = CtxClient(w.url, KEY, timeout=TIMEOUT_S)
    _, error, took = _timed(lambda: client.search("q"))
    assert isinstance(error, CtxApiError) and error.status == 302 and "redirect not followed" in str(error)
    assert took < SLACK * TIMEOUT_S, took
    assert _wait_for(lambda: bool(w.closed_by_client), SLACK * TIMEOUT_S), "the double never saw the close"


@pytest.mark.parametrize(
    "plan",
    [
        lambda cap: endless(24 * cap),
        lambda cap: endless(24 * cap, chunked=True),
        lambda cap: endless(24 * cap, length=1 << 40),
    ],
    ids=["until_closed", "chunked", "announced_length"],
)
def test_an_answer_over_the_size_cap_ends_as_an_error_not_in_memory(wire, monkeypatch, plan):
    """A server that sends without end (or announces more than the cap) ends the call with an error
    once the cap is passed; the client stops reading, so the double could send at most the cap plus
    what the socket buffers hold."""
    cap = 2 * MIB
    monkeypatch.setattr(client_mod, "MAX_ANSWER_BYTES", cap, raising=False)
    w = wire(lambda *_: plan(cap))
    client = CtxClient(w.url, KEY, timeout=30.0)
    data, error, _ = _timed(lambda: client.search("q"))
    assert isinstance(error, CtxApiError), data
    assert error.status is None and error.code == "" and str(error).startswith("connection error: answer"), str(error)
    assert _wait_for(lambda: bool(w.sent), 10.0)
    assert w.sent[0] < cap + 16 * MIB < 24 * cap, w.sent


def test_an_announced_length_over_the_cap_is_refused_before_the_body(wire, monkeypatch):
    """A ``Content-Length`` over the cap ends the call when the head arrives: no byte of the body is
    waited for (here none comes)."""
    cap = 2 * MIB
    monkeypatch.setattr(client_mod, "MAX_ANSWER_BYTES", cap, raising=False)

    def plan(*_):
        yield _head(200, cap + 1)
        yield TRICKLE_S

    w = wire(plan)
    client = CtxClient(w.url, KEY, timeout=SLACK * TIMEOUT_S)
    _, error, took = _timed(lambda: client.search("q"))
    assert isinstance(error, CtxApiError) and str(error).startswith("connection error: answer"), error
    assert took < TIMEOUT_S, took


# -- the client: what stays as it was ------------------------------------------------------------


@pytest.mark.parametrize("cut", ["after_the_json", "inside_the_json"])
def test_an_answer_shorter_than_its_content_length_is_still_an_error(wire, cut):
    """The connection ends before the announced body is complete (a restart of ctx or a proxy that
    drops the connection): the call raises ``IncompleteRead`` as before — a whole JSON document
    followed by a missing tail is no answer, and half a document is no failure envelope."""
    body = json.dumps(SEARCH_ANSWER).encode()

    def plan(*_):
        if cut == "after_the_json":
            yield _head(200, len(body) + 100) + body
        else:
            yield _head(200, len(body)) + body[: len(body) // 2]

    w = wire(plan)
    client = CtxClient(w.url, KEY, timeout=SLACK * TIMEOUT_S)
    data, error, _ = _timed(lambda: client.search("q"))
    assert isinstance(error, CtxApiError), data
    assert str(error).startswith("connection error: IncompleteRead("), str(error)
    assert (error.status, error.code) == (None, "")


def test_a_truncated_query_answer_lets_the_prefetch_fall_back_to_search(wire, tmp_path, monkeypatch):
    def plan(method, path, body):
        if path == "/api/whoami":
            return _whoami()
        if path == "/api/query":
            raw = json.dumps({"success": True, "sources": MAIN}).encode()
            return iter([_head(200, len(raw)) + raw[: len(raw) // 2]])
        return answer({"success": True, "results": MAIN[:1], "count": 1})

    w = wire(plan)
    p = _rest_provider(tmp_path, monkeypatch, w.url)
    assert _titles(p.prefetch("where is the proxy?")) == [MAIN[0]["title"]]
    assert w.paths() == ["/api/whoami", "/api/query", "/api/search"]
    p.shutdown()


SLOW_BOUND_S = 4.0  # the bound of the calls that must still arrive


@pytest.mark.parametrize(
    "plan",
    [
        lambda: late(SEARCH_ANSWER, 0.8 * SLOW_BOUND_S),
        lambda: trickled_body(SEARCH_ANSWER, seconds=0.4 * SLOW_BOUND_S),
        lambda: trickled_chunks(SEARCH_ANSWER, seconds=0.4 * SLOW_BOUND_S),
        lambda: answer(SEARCH_ANSWER),
    ],
    ids=["head_at_80_percent", "body_trickles_within", "chunks_within", "at_once"],
)
def test_a_slow_answer_that_completes_within_the_timeout_arrives(wire, plan):
    w = wire(lambda *_: plan())
    client = CtxClient(w.url, KEY, timeout=SLOW_BOUND_S)
    assert client.search("q") == SEARCH_ANSWER


def test_the_largest_answer_of_a_plugin_call_arrives_whole(wire):
    """The largest answer measured for a call of the plugin: a search at its largest limit (10) whose
    every row carries the tags of a block at ctx's largest single write — ~6 MB each on the wire,
    every character escaped to six bytes (59.8 MB measured against ctx's handlers)."""
    tags = ["\x01" * 996_000]
    rows = [{"id": f"b{i}", "title": "t", "category": "c", "tags": tags} for i in range(10)]
    w = wire(lambda *_: answer({"success": True, "results": rows, "count": 10}))
    client = CtxClient(w.url, KEY, timeout=60.0)
    data = client.search("q", limit=10)
    assert [r["tags"] == tags for r in data["results"]] == [True] * 10
    assert _wait_for(lambda: bool(w.sent), 10.0)
    assert w.sent[0] > 59_000_000, w.sent


def test_get_block_sends_its_fallback_within_the_one_bound(wire):
    """An older server refuses the top-level ``id``; the second form is sent with what is left of
    the call's bound, not with a bound of its own: each answer takes 60 % of the bound, so the
    second ends the call at its bound (before, it got a whole bound again and arrived)."""

    def plan(method, path, body):
        if "id" in body:
            return late({"success": False, "error": "Missing required field: id"}, 0.6 * TIMEOUT_S, status=400)
        return late({"success": True, "block": {"id": "b1"}}, 0.6 * TIMEOUT_S)

    w = wire(plan)
    client = CtxClient(w.url, KEY, timeout=TIMEOUT_S)
    data, error, took = _timed(lambda: client.get_block("b1"))
    assert isinstance(error, CtxApiError) and "timed out" in str(error), data
    assert w.paths() == ["/api/manage", "/api/manage"]
    assert took < SLACK * TIMEOUT_S, took


# -- the recall: within Hermes' wait for the prefetch ----------------------------------------------

MAIN = [{"id": f"k{i}", "title": f"Knowledge {i}", "category": "reference", "content": f"k {i}"} for i in range(5)]
TYPES = {"success": True, "types": [{"name": n} for n in ("knowledge", "checkpoint", SESSION_DIGEST_TYPE)]}
TYPED = [{"id": "f1", "title": "fact: the queue drains at 03:00", "category": "learnings", "untrusted": True}]


def _rest_provider(tmp_path, monkeypatch, url: str, config: dict | None = None):
    from ctx_plugin.provider import CtxMemoryProvider

    monkeypatch.setenv("CTX_KEY", KEY)
    monkeypatch.setenv("CTX_BASE_URL", url)
    home = tmp_path / "profiles" / "deadline"
    home.mkdir(parents=True, exist_ok=True)
    (home / "ctx.json").write_text(json.dumps(config or {}), encoding="utf-8")
    provider = CtxMemoryProvider()
    provider.initialize("sess-566", hermes_home=str(home), agent_context="primary")
    assert provider._active and isinstance(provider._client, CtxClient)
    return provider


def _whoami() -> Iterator[Step]:
    yield from answer({"success": True, "home_scope": "shared", "label": "test-key"})


def _titles(context: str) -> list[str]:
    return [line.rsplit("] ", 1)[-1] for line in context.splitlines() if line.startswith("- ")]


def test_the_prefetch_keeps_the_recalled_blocks_when_only_the_second_query_trickles(wire, tmp_path, monkeypatch):
    """The second query (blocks of type session-digest) trickles: it ends at its own bound and the
    five recalled blocks come back. Before, the prefetch waited for the trickle — past Hermes' wait,
    which then dropped the five as well."""
    bound = TIMEOUT_S
    monkeypatch.setattr(provider_mod, "SESSION_DIGEST_RECALL_TIMEOUT_S", bound)

    def plan(method, path, body):
        if path == "/api/whoami":
            return _whoami()
        if path == "/api/manage":
            return answer(TYPES)
        if "limit" in body:  # the second query
            return trickled_body({"success": True, "sources": TYPED})
        return answer({"success": True, "sources": MAIN})

    w = wire(plan)
    p = _rest_provider(tmp_path, monkeypatch, w.url)
    _cp.entry_state(p._entry_key()).type_answered(SESSION_DIGEST_TYPE, True)
    context, _, took = _timed(lambda: p.prefetch("when does the queue drain?"))
    assert _titles(context) == [s["title"] for s in MAIN]
    assert took < SLACK * bound, took
    p.shutdown()


def test_the_prefetch_ends_before_hermes_stops_waiting_when_the_query_trickles(wire, tmp_path, monkeypatch):
    """The recall's own query trickles: the prefetch ends within what Hermes' wait leaves (less the
    margin) with the recall a failed query gives — nothing — instead of running past the wait, and
    no fallback search is sent with no time left for it."""
    monkeypatch.setattr(provider_mod, "HOST_PREFETCH_WAIT_S", 2 * provider_mod.SESSION_DIGEST_RECALL_MARGIN_S)
    expected = provider_mod.HOST_PREFETCH_WAIT_S - provider_mod.SESSION_DIGEST_RECALL_MARGIN_S

    def plan(method, path, body):
        if path == "/api/whoami":
            return _whoami()
        if path == "/api/query":
            return trickled_body({"success": True, "sources": MAIN})
        return answer(SEARCH_ANSWER)

    w = wire(plan)
    p = _rest_provider(tmp_path, monkeypatch, w.url, {"api_timeout": 5})
    context, _, took = _timed(lambda: p.prefetch("where is the proxy?"))
    assert context == ""
    assert took < SLACK * expected, took
    assert w.paths() == ["/api/whoami", "/api/query"]
    p.shutdown()


class _Clocked(FakeClient):
    """Records the bound of each read; the query costs ``query_s`` on the provider's clock."""

    clock: ClassVar[dict] = {}
    query_s = 0.0
    transport = "rest"

    def query(self, text, **k):
        _Clocked.clock["now"] += _Clocked.query_s
        return super().query(text, **k)


@pytest.fixture
def clocked(monkeypatch, fake_client_cls):
    _Clocked.clock = {"now": 1_000.0}
    _Clocked.query_s = 0.0
    _Clocked.transport = "rest"
    monkeypatch.setattr(provider_mod, "CtxClient", _Clocked)
    return _Clocked


@pytest.mark.parametrize("api_timeout", [10.0, 3.0])
def test_the_prefetch_bounds_its_query_and_fallback_by_what_hermes_wait_leaves(make_provider, clocked, api_timeout):
    """Over REST the prefetch's query gets at most what Hermes' wait leaves less the margin, and a
    fallback search after a failed query what is left of that; neither gets more than api_timeout."""
    p, c = make_provider(config={"api_timeout": api_timeout})
    p._now = lambda: clocked.clock["now"]
    clocked.query_s = 3.0
    c.query_raises = CtxApiError("connection error: timed out")
    p.prefetch("where is the proxy?")
    (q_kind, _, q_kw), (s_kind, _, s_kw) = c.reads
    wait = provider_mod.HOST_PREFETCH_WAIT_S - provider_mod.SESSION_DIGEST_RECALL_MARGIN_S
    assert (q_kind, s_kind) == ("query", "search")
    assert q_kw["timeout"] == pytest.approx(min(api_timeout, wait))
    assert s_kw["timeout"] == pytest.approx(min(api_timeout, wait - 3.0))
    p.shutdown()


def test_the_prefetch_sends_no_fallback_search_when_the_query_used_hermes_wait(make_provider, clocked):
    p, c = make_provider()
    p._now = lambda: clocked.clock["now"]
    clocked.query_s = provider_mod.HOST_PREFETCH_WAIT_S
    c.query_raises = CtxApiError("connection error: timed out")
    assert p.prefetch("where is the proxy?") == ""
    assert [r[0] for r in c.reads] == ["query"]
    p.shutdown()


def test_the_search_mode_prefetch_is_bounded_by_hermes_wait(make_provider, clocked):
    p, c = make_provider(config={"prefetch_mode": "search"})
    p._now = lambda: clocked.clock["now"]
    p.prefetch("where is the proxy?")
    ((kind, _, kw),) = c.reads
    wait = provider_mod.HOST_PREFETCH_WAIT_S - provider_mod.SESSION_DIGEST_RECALL_MARGIN_S
    assert kind == "search" and kw["timeout"] == pytest.approx(min(10.0, wait))
    p.shutdown()


def test_a_warm_up_and_the_mcp_transport_keep_their_bounds(make_provider, clocked):
    """A warm-up (queue_prefetch) has no Hermes wait behind it: its query keeps api_timeout, so an
    answer it waited for before still arrives. Over MCP the transport owns the recall's timeout, as
    before."""
    p, c = make_provider()
    p.queue_prefetch("where is the proxy?")
    p._queue_thread.join(5.0)
    clocked.transport = "mcp"
    p.prefetch("and the queue?")
    assert [(kind, "timeout" in kw) for kind, _, kw in c.reads] == [("query", False), ("query", False)]
    p.shutdown()


# -- the checkpoint: within its budget, fail-closed ----------------------------------------------


class CtxRest:
    """ctx's REST surface as the chain writer uses it: upsert by (category, title), an identical
    store answered ``noop`` before anything is booked, search by title and tags, get by id.
    ``stall`` (``"silent"`` or ``"trickled"``): the next store is stored, its answer stalls."""

    def __init__(self) -> None:
        self.blocks: dict[tuple[str, str], dict] = {}
        self.counter = 0
        self.noops = 0
        self.stall: str | None = None

    def plan(self, method: str, path: str, body: Any) -> Iterator[Step]:
        if path == "/api/whoami":
            return _whoami()
        if path == "/api/search":
            return answer(self._search(body))
        if path == "/api/manage":
            return answer(self._get(body))
        assert path == "/api/store", path
        result = self._store(body)
        stall, self.stall = self.stall, None
        if stall == "silent":
            return silent_after_head(result)
        if stall == "trickled":
            return trickled_body(result)
        return answer(result)

    def _store(self, body: dict) -> dict:
        key = (body["category"], body["title"])
        incoming = {k: body.get(k) for k in ("content", "tags", "sensitivity", "type")}
        stored = self.blocks.get(key)
        metadata = body.get("metadata") or {}
        if (
            stored is not None
            and all(stored[k] == v for k, v in incoming.items())
            and all(stored["metadata"].get(k) == v for k, v in metadata.items())
        ):
            self.noops += 1
            return {"action": "noop", "existing_id": stored["id"], "reason": "identical content"}
        created = stored is None
        if created:
            self.counter += 1
            stored = {"id": f"019f8000-0000-7000-8000-{self.counter:012d}", "created_at": f"t{self.counter}"}
            self.blocks[key] = stored
        stored.update(incoming, metadata=dict(metadata), title=body["title"], category=key[0])
        stored["updated_at"] = stored["created_at"] if created else f"u{self.counter}-{len(self.blocks)}"
        block = {k: stored[k] for k in ("id", "title", "category", "created_at", "updated_at")}
        return {"success": True, "block": block}

    def _search(self, body: dict) -> dict:
        tags = set(body.get("tags") or [])
        rows = [
            {"id": b["id"], "title": b["title"], "category": b["category"], "type": b["type"]}
            for b in self.blocks.values()
            if body["query"] in b["title"] and tags <= set(b["tags"] or [])
        ]
        rows = rows[: int(body.get("limit") or 10)]
        return {"success": True, "results": rows, "count": len(rows)}

    def _get(self, body: dict) -> dict:
        for b in self.blocks.values():
            if b["id"] == body.get("id"):
                return {"success": True, "block": dict(b)}
        return {"success": False, "error": "block not found"}


def _turns(tag: str) -> list[dict]:
    return [{"role": "user", "content": f"{tag} " + "x" * 400}]  # one part + manifest + head


@pytest.mark.parametrize("stall", ["silent", "trickled"])
def test_a_write_given_up_that_landed_reads_as_a_timeout_did(wire, tmp_path, monkeypatch, stall):
    """The chain's first write reaches ctx and is stored, but its answer does not come within the
    call's bound (api_timeout, inside the budget): silent (a timeout, before and after) or trickled
    (before: waited for until the run was past its budget; now: given up at the bound). The required
    checkpoint fails closed at the bound either way, and the next one finds the part ctx holds,
    confirmed as identical (no second block, nothing booked twice), and completes — the same for
    both stalls."""
    rest = CtxRest()
    rest.stall = stall
    w = wire(rest.plan)
    api_timeout = 1.0
    p = _rest_provider(tmp_path, monkeypatch, w.url, {"api_timeout": api_timeout, "checkpoint_budget_s": 5})
    messages = _turns("a")
    _, error, took = _timed(lambda: p.on_pre_compress(messages, require_checkpoint=True))
    assert isinstance(error, _cp.CtxCheckpointError), error
    assert took < SLACK * api_timeout, took
    ((_, only),) = rest.blocks
    assert " part " in only, only

    assert "ctx checkpoint head:" in p.on_pre_compress(messages, require_checkpoint=True)
    titles = [title for _, title in rest.blocks]
    assert len(titles) == 3 and sum(" part " in t for t in titles) == 1, titles
    assert rest.noops == 1 and p._last_chain_receipt.reused == 1
    p.shutdown()


# -- review round 1: a dead first address, the exact bounds, TLS, the cap's value -------------------


def _resolving(monkeypatch, host: str, addresses: list[tuple[str, int]]) -> None:
    """``host`` resolves to ``addresses`` in this order (any other name as before)."""
    real = socket.getaddrinfo

    def getaddrinfo(name, port, *args, **kwargs):
        if name == host:
            return [(socket.AF_INET, socket.SOCK_STREAM, 6, "", a) for a in addresses]
        return real(name, port, *args, **kwargs)

    monkeypatch.setattr(socket, "getaddrinfo", getaddrinfo)


def _address(w: Wire) -> tuple[str, int]:
    return "127.0.0.1", int(w.url.rsplit(":", 1)[1])


@pytest.mark.parametrize("dead", [1, 2])
def test_a_host_whose_first_addresses_never_answer_is_reached_on_its_live_one(wire, full_backlog, monkeypatch, dead):
    """A name that resolves first to addresses that never answer the connect (an IPv6 address without
    a route, say) and then to a live one: each address gets a share of the time left, so the live one
    is still tried within the bound. Before the share, the first dead address took the whole bound and
    the call failed where the stdlib's connect (every address the whole timeout) had only been slow."""
    w = wire(lambda *_: answer(SEARCH_ANSWER))
    _resolving(monkeypatch, "dual.test", [full_backlog() for _ in range(dead)] + [_address(w)])
    client = CtxClient("http://dual.test:8080", KEY, timeout=3 * TIMEOUT_S)
    assert client.search("q") == SEARCH_ANSWER
    assert w.paths() == ["/api/search"]


@pytest.fixture
def spy(monkeypatch):
    """Every socket timeout the client sets, with the time it was set: the client's ``socket`` module
    seen through a socket class that records its ``settimeout`` calls (the double's sockets are not)."""
    sets: list[tuple[float, float]] = []

    class Recording(socket.socket):
        __slots__ = ()

        def settimeout(self, value):
            sets.append((time.monotonic(), value))
            super().settimeout(value)

    class View:
        """The socket module as the client sees it, read at each use (a patched ``getaddrinfo`` too)."""

        socket = Recording

        def __getattr__(self, name):
            return getattr(socket, name)

    monkeypatch.setattr(client_mod, "socket", View())
    return sets


def _assert_within(sets: list[tuple[float, float]], bound: float, eps: float = 0.05) -> None:
    """The exact measure: no timeout the client set reaches past the call's deadline. The deadline is
    fixed when the connection is made, before the first timeout is set, so it lies at most ``bound``
    after that first one: every timeout set at ``t`` with value ``v`` has ``t + v`` within it."""
    assert sets, "the client set no socket timeout"
    first = sets[0][0]
    late = [(round(t - first, 3), round(v, 3)) for t, v in sets if t + v > first + bound + eps]
    assert not late, late


def test_every_timeout_set_for_a_trickling_answer_ends_within_the_bound(wire, spy):
    w = wire(lambda *_: trickled_body(SEARCH_ANSWER))
    _, error, _ = _timed(lambda: CtxClient(w.url, KEY, timeout=TIMEOUT_S).search("q"))
    assert isinstance(error, CtxApiError)
    assert len(spy) > 3  # the connect, then one per read
    _assert_within(spy, TIMEOUT_S)


def test_dead_addresses_share_the_bound_and_the_call_ends_at_it(full_backlog, monkeypatch, spy):
    """Three addresses that never answer: the first gets a third of the bound, the next ones what is
    left divided among them, and the call ends at the bound — not before (the last address gets all
    that is left), not after."""
    _resolving(monkeypatch, "dead.test", [full_backlog() for _ in range(3)])
    bound = 3 * TIMEOUT_S
    _, error, took = _timed(lambda: CtxClient("http://dead.test:8080", KEY, timeout=bound).search("q"))
    assert isinstance(error, CtxApiError) and "timed out" in str(error), error
    assert took >= 0.9 * bound, took
    assert spy[0][1] <= bound / 3 + 0.01, spy
    _assert_within(spy, bound)


def test_more_dead_addresses_than_the_bound_has_room_for_get_the_least_share_each(full_backlog, monkeypatch, spy):
    """Six addresses that never answer and a bound of half a second: an equal share would be 83 ms; each
    gets at least ``CONNECT_SHARE_MIN_S`` (a connect over a slow route still fits), the addresses after
    the bound are not tried, and the call ends at the bound."""
    _resolving(monkeypatch, "six.dead.test", [full_backlog() for _ in range(6)])
    _, error, took = _timed(lambda: CtxClient("http://six.dead.test:8080", KEY, timeout=TIMEOUT_S).search("q"))
    assert isinstance(error, CtxApiError) and "timed out" in str(error), error
    shares = [round(v, 3) for _, v in spy]
    assert shares[0] >= client_mod.CONNECT_SHARE_MIN_S and len(shares) <= 5, shares
    assert TIMEOUT_S * 0.9 <= took < SLACK * TIMEOUT_S, took
    _assert_within(spy, TIMEOUT_S)


def test_a_connect_to_a_live_address_after_a_dead_one_keeps_the_rest_of_the_bound(wire, full_backlog, monkeypatch, spy):
    w = wire(lambda *_: answer(SEARCH_ANSWER))
    _resolving(monkeypatch, "dual.test", [full_backlog(), _address(w)])
    bound = 3 * TIMEOUT_S
    assert CtxClient("http://dual.test:8080", KEY, timeout=bound).search("q") == SEARCH_ANSWER
    _assert_within(spy, bound)


@pytest.mark.parametrize("length, refused", [(64 * MIB + 1, True), (64 * MIB, False)])
def test_the_size_cap_is_64_mib(wire, length, refused):
    """At the real cap, without sending it: a ``Content-Length`` one byte over 64 MiB is refused when the
    head arrives; one of exactly 64 MiB is waited for (here it never comes, so the call times out)."""

    def plan(*_):
        yield _head(200, length)
        yield TRICKLE_S

    w = wire(plan)
    _, error, took = _timed(lambda: CtxClient(w.url, KEY, timeout=TIMEOUT_S).search("q"))
    assert isinstance(error, CtxApiError)
    if refused:
        assert str(error).startswith("connection error: answer larger than 67108864 bytes"), str(error)
        assert took < TIMEOUT_S, took
    else:
        assert str(error) == "connection error: timed out", str(error)


@pytest.fixture(scope="module")
def tls_files(tmp_path_factory):
    """A CA and a server certificate for 127.0.0.1 and localhost, made for this module (the extensions
    the strict X.509 checks of Python 3.13+ ask for)."""
    x509 = pytest.importorskip("cryptography.x509")
    from cryptography.hazmat.primitives import hashes, serialization
    from cryptography.hazmat.primitives.asymmetric import ec
    from cryptography.x509.oid import ExtendedKeyUsageOID, NameOID

    now = datetime.datetime.now(datetime.timezone.utc)
    usage = dict(content_commitment=False, key_encipherment=False, data_encipherment=False, key_agreement=False)
    usage.update(encipher_only=False, decipher_only=False)

    def build(subject, issuer, key, signer, extensions):
        builder = (
            x509.CertificateBuilder()
            .subject_name(subject)
            .issuer_name(issuer)
            .public_key(key.public_key())
            .serial_number(x509.random_serial_number())
            .not_valid_before(now - datetime.timedelta(minutes=5))
            .not_valid_after(now + datetime.timedelta(days=1))
        )
        for extension, critical in extensions:
            builder = builder.add_extension(extension, critical=critical)
        return builder.sign(signer, hashes.SHA256())

    ca_key, key = ec.generate_private_key(ec.SECP256R1()), ec.generate_private_key(ec.SECP256R1())
    ca_name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "ctx_checkpoint test CA")])
    ca = build(
        ca_name,
        ca_name,
        ca_key,
        ca_key,
        [
            (x509.BasicConstraints(ca=True, path_length=None), True),
            (x509.KeyUsage(digital_signature=True, key_cert_sign=True, crl_sign=True, **usage), True),
            (x509.SubjectKeyIdentifier.from_public_key(ca_key.public_key()), False),
        ],
    )
    leaf = build(
        x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "localhost")]),
        ca_name,
        key,
        ca_key,
        [
            (
                x509.SubjectAlternativeName(
                    [x509.DNSName("localhost"), x509.IPAddress(ipaddress.ip_address("127.0.0.1"))]
                ),
                False,
            ),
            (x509.BasicConstraints(ca=False, path_length=None), True),
            (x509.KeyUsage(digital_signature=True, key_cert_sign=False, crl_sign=False, **usage), True),
            (x509.ExtendedKeyUsage([ExtendedKeyUsageOID.SERVER_AUTH]), False),
            (x509.SubjectKeyIdentifier.from_public_key(key.public_key()), False),
            (x509.AuthorityKeyIdentifier.from_issuer_public_key(ca_key.public_key()), False),
        ],
    )
    folder = tmp_path_factory.mktemp("tls")
    pem = serialization.Encoding.PEM
    (folder / "ca.pem").write_bytes(ca.public_bytes(pem))
    (folder / "server.pem").write_bytes(leaf.public_bytes(pem))
    (folder / "server.key").write_bytes(
        key.private_bytes(pem, serialization.PrivateFormat.PKCS8, serialization.NoEncryption())
    )
    return folder


@pytest.fixture
def tls(tls_files, monkeypatch):
    """The server's TLS context; the client trusts the test CA the way it trusts any: ``SSL_CERT_FILE``,
    which urllib's HTTPS handler reads when the opener is built (in Hermes: when the plugin loads), so
    the opener is built again as the module builds it. No verification is turned off."""
    monkeypatch.setenv("SSL_CERT_FILE", str(tls_files / "ca.pem"))
    monkeypatch.setattr(client_mod, "_OPENER", urllib.request.build_opener(*client_mod._HANDLERS))
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(tls_files / "server.pem", tls_files / "server.key")
    return context


def _https(w: Wire, host: str = "127.0.0.1") -> str:
    return f"https://{host}:{_address(w)[1]}"


def test_over_tls_an_answer_arrives_and_a_trickling_one_ends_at_the_bound(wire, tls):
    ok = wire(lambda *_: answer(SEARCH_ANSWER), tls=tls)
    assert CtxClient(_https(ok), KEY, timeout=SLOW_BOUND_S).search("q") == SEARCH_ANSWER
    w = wire(lambda *_: trickled_body(SEARCH_ANSWER), tls=tls)
    _, error, took = _timed(lambda: CtxClient(_https(w), KEY, timeout=TIMEOUT_S).search("q"))
    assert isinstance(error, CtxApiError) and "timed out" in str(error), error
    assert (error.status, error.code) == (None, "")
    assert TIMEOUT_S * 0.9 <= took < SLACK * TIMEOUT_S, took
    assert _wait_for(lambda: bool(w.closed_by_client), SLACK * TIMEOUT_S), "the double never saw the close"


def test_over_tls_a_handshake_longer_than_the_connect_share_keeps_the_rest_of_the_bound(
    wire, tls, full_backlog, monkeypatch
):
    """The live address comes first of two, so its connect gets half the bound; the handshake after it
    takes longer than that half and still has the rest of the bound."""
    bound = SLOW_BOUND_S
    w = wire(lambda *_: answer(SEARCH_ANSWER), tls=tls, handshake_delay=0.7 * bound)
    _resolving(monkeypatch, "localhost", [_address(w), full_backlog()])
    assert CtxClient(_https(w, "localhost"), KEY, timeout=bound).search("q") == SEARCH_ANSWER
