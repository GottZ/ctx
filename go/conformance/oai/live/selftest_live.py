"""Self-test of the live tools (smoke.py, measure.py, checklist.json, test.sh
T25) against fakes of a running ctx — waves W02-15, W06-19 stage 2, W06-20.

Not collected by the in-process harness (``pytest conformance/oai`` collects
``test_*.py`` only); run explicitly, locally and in the nightly CI job
``gateway-sdk-smoke``:

    cd go && uv run --no-project --with openai==3.42.1 --with pytest==9.2.0 \\
        pytest -p no:cacheprovider -q conformance/oai/live/selftest_live.py

Every test names its gate. Nothing here talks to a real ctx or engine.
"""

import json
import os
import pathlib
import re
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

LIVE = pathlib.Path(__file__).resolve().parent
REPO = LIVE.parents[3]
SMOKE = LIVE / "smoke.py"
MEASURE = LIVE / "measure.py"
KEY = "ctxm_selftest"
ADMIN = "selftest-admin-key"


def _envelope(code, etype, message="m"):
    return {"error": {"message": message, "type": etype, "param": None, "code": code}}


class Fake:
    """A fake ctx. mode: off | on | door-first | proxy-unset | house-errors |
    bad-chat | retry-ok | retry-bad | slow-prefill | cut | heap |
    no-ratelimit.

    In mode heap every stream takes ~0.6 s and the fake writes its own
    "container memory" to ``mem_file``: 100 MB + 20 MB per open stream, so
    the measured peak follows the flood instead of a timer (S2: the S1 test
    raced a writer thread against the tool's start)."""

    MEM_BASE, MEM_PER_STREAM = 100_000_000, 20_000_000

    def __init__(self, mode):
        self.mode = mode
        self.calls = []
        self.n = 0
        self.open_streams = 0
        self.mem_file = None
        self.foreign_load = []
        self.backends = []
        self.lock = threading.Lock()
        fake = self

        class H(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *a):
                pass

            def _send(self, status, body, headers=None, ctype="application/json"):
                raw = body if isinstance(body, bytes) else json.dumps(body).encode()
                self.send_response(status)
                self.send_header("Content-Type", ctype)
                self.send_header("Content-Length", str(len(raw)))
                for k, v in (headers or {}).items():
                    self.send_header(k, v)
                self.end_headers()
                self.wfile.write(raw)

            def _body(self):
                n = int(self.headers.get("Content-Length") or 0)
                return json.loads(self.rfile.read(n) or b"{}") if n else {}

            def do_GET(self):
                fake.route(self, "GET", {})

            def do_POST(self):
                fake.route(self, "POST", self._body())

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.url = f"http://127.0.0.1:{self.srv.server_address[1]}"
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def close(self):
        self.srv.shutdown()

    def count(self, what):
        with self.lock:
            return sum(1 for c in self.calls if c == what)

    def set_mem(self, delta):
        with self.lock:
            self.open_streams += delta
            if self.mem_file is not None:
                self.mem_file.write_text(f"{self.MEM_BASE + self.MEM_PER_STREAM * self.open_streams}\n")

    def route(self, h, method, body):
        path = h.path.split("?")[0]
        with self.lock:
            self.calls.append(body.get("action") if path == "/api/manage" else f"{method} {path}")
            self.n += 1
            n = self.n
        if path == "/api/status":
            if h.headers.get("X-Context-Key") != ADMIN:
                return h._send(403, {"success": False, "error": "forbidden"})
            return h._send(200, {"foreign_load": self.foreign_load})
        if path == "/api/manage":
            if h.headers.get("X-Context-Key") != ADMIN:
                return h._send(403, {"success": False, "error": "forbidden"})
            if body.get("action") == "backend-list":
                return h._send(200, {"success": True, "backends": self.backends})
            if body.get("action") == "model-consumer-create":
                return h._send(200, {"success": True, "consumer": {"id": "c-1"}, "api_key": KEY})
            if body.get("action") == "model-consumer-delete":
                return h._send(200, {"success": True, "cancelled_inflight": 0})
            return h._send(400, {"success": False, "error": "unknown action"})
        if path == "/health":
            return h._send(200, {"status": "ok", "version": "5.33.0"})
        if not (path.startswith("/v1/") or path.startswith("/v2/")):
            return h._send(404, b"404 page not found", ctype="text/plain")
        nf = (404, _envelope("not_found", "invalid_request_error"), {"x-should-retry": "false"})
        unauth = (401, _envelope("invalid_api_key", "invalid_request_error"),
                  {"x-should-retry": "false", "WWW-Authenticate": 'Bearer realm="ctx"'})
        bearer = h.headers.get("Authorization", "")
        if self.mode == "proxy-unset":
            return h._send(503, _envelope("proxy_misconfigured", "service_unavailable_error"), {"x-should-retry": "false"})
        if self.mode == "off":
            return h._send(*nf)
        if self.mode == "door-first" and bearer != f"Bearer {KEY}":
            return h._send(*unauth)
        if self.mode == "door-first":
            return h._send(*nf)
        if self.mode in ("retry-ok", "retry-bad"):
            if bearer != f"Bearer {KEY}":
                return h._send(*unauth)
            if n % 2:
                ra = "5" if self.mode == "retry-ok" else "45"
                return h._send(429, _envelope("rate_limit_exceeded", "rate_limit_error"),
                               {"x-should-retry": "true", "Retry-After": ra})
            return h._send(200, _chat())
        if bearer != f"Bearer {KEY}":
            if self.mode == "house-errors":
                return h._send(401, {"success": False, "error": "unauthorized"})
            return h._send(*unauth)
        if method == "GET" and path == "/v1/models":
            return h._send(200, {"object": "list", "data": [
                {"id": m, "object": "model", "created": 0, "owned_by": "ctx", "ctx": {"kind": k}}
                for m, k in (("chat", "chat"), ("embed", "embedding"), ("rerank", "rerank"))]})
        if method == "POST" and path == "/v1/chat/completions":
            if self.mode == "bad-chat":
                return h._send(200, {"choices": "nope"})
            if body.get("stream"):
                return _stream(h, self)
            if self.mode == "no-ratelimit":
                return h._send(200, _chat())
            return h._send(200, _chat(), {f"x-ratelimit-{k}-{d}": v for k, v in (("limit", "30"), ("remaining", "29"), ("reset", "2s"))
                                          for d in ("requests", "tokens")})
        if method == "POST" and path == "/v1/embeddings":
            inputs = body.get("input")
            inputs = inputs if isinstance(inputs, list) else [inputs]
            return h._send(200, {"object": "list", "model": "embed",
                                 "data": [{"object": "embedding", "index": i, "embedding": [0.1, 0.2]} for i in range(len(inputs))],
                                 "usage": {"prompt_tokens": 2, "total_tokens": 2}})
        if method == "POST" and path in ("/v1/rerank", "/v2/rerank"):
            docs = body.get("documents") or []
            return h._send(200, {"id": "r", "model": "rerank",
                                 "results": [{"index": i, "relevance_score": round(0.9 - 0.1 * i, 3)} for i in range(len(docs))],
                                 "usage": {"prompt_tokens": 3, "total_tokens": 3}})
        return h._send(*nf)


def _chat():
    return {"id": "c", "object": "chat.completion", "created": 0, "model": "chat",
            "choices": [{"index": 0, "finish_reason": "stop", "logprobs": None,
                         "message": {"role": "assistant", "content": "hi", "refusal": None}}],
            "usage": {"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4}}


def _stream(h, fake=None):
    mode = fake.mode if fake else "on"
    h.send_response(200)
    h.send_header("Content-Type", "text/event-stream")
    h.send_header("Connection", "close")
    h.end_headers()
    h.close_connection = True
    if mode == "cut":
        h.wfile.write(b": keepalive\n\n")
        h.wfile.flush()
        return
    if mode == "slow-prefill":
        h.wfile.write(b": keepalive\n\n")
        h.wfile.flush()
        time.sleep(0.4)
    if mode == "heap":
        fake.set_mem(+1)
        time.sleep(0.6)
    base = {"id": "c", "object": "chat.completion.chunk", "created": 0, "model": "chat"}
    frames = [
        dict(base, choices=[{"index": 0, "delta": {"role": "assistant", "content": "h"}, "finish_reason": None}]),
        dict(base, choices=[{"index": 0, "delta": {"content": "i"}, "finish_reason": "stop"}]),
        dict(base, choices=[], usage={"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4}),
    ]
    for f in frames:
        h.wfile.write(b"data: " + json.dumps(f).encode() + b"\n\n")
        h.wfile.flush()
    h.wfile.write(b"data: [DONE]\n\n")
    h.wfile.flush()
    if mode == "heap":
        fake.set_mem(-1)


@pytest.fixture
def fake(request):
    f = Fake(request.param)
    yield f
    f.close()


def _run(script, *args, env=None):
    e = dict(os.environ, CTX_SELFTEST_ADMIN=ADMIN, CTX_SELFTEST_KEY=KEY, **(env or {}))
    p = subprocess.run([sys.executable, str(script), *args], capture_output=True, text=True, timeout=120, env=e)
    lines = [l for l in p.stdout.strip().splitlines() if l.strip()]
    return p.returncode, (lines[-1] if lines else ""), p.stdout + p.stderr


@pytest.mark.parametrize("fake", ["off"], indirect=True)
def test_t25_skips_without_flag(fake):
    """W02-15 gate: T25 without the flag is SKIP with its reason, never PASS —
    and no test consumer is created."""
    rc, last, out = _run(SMOKE, "t25", "--base-url", fake.url, "--admin-key-env", "CTX_SELFTEST_ADMIN")
    assert rc == 77, f"T25 with gateway.enabled=false exited {rc} ({last!r}), want 77 SKIP — never PASS\n{out}"
    assert last.startswith("SKIP:") and "gateway.enabled" in last, f"SKIP line must name gateway.enabled: {last!r}"
    assert fake.count("model-consumer-create") == 0, "T25 created a consumer while the flag is off"


@pytest.mark.parametrize("fake", ["on"], indirect=True)
def test_t25_passes_and_cleans_up(fake):
    """W02-15 gate: with the flag on T25 creates a test consumer, runs every
    probe (models, chat, chat stream, embeddings, rerank, 401 without key)
    and deletes the consumer."""
    rc, last, out = _run(SMOKE, "t25", "--base-url", fake.url, "--admin-key-env", "CTX_SELFTEST_ADMIN")
    assert rc == 0 and last.startswith("PASS"), f"T25 against a conformant stack: {rc} {last!r}, want 0 PASS\n{out}"
    assert fake.count("model-consumer-create") == 1 and fake.count("model-consumer-delete") == 1, fake.calls
    for probe in ("GET /v1/models", "POST /v1/chat/completions", "POST /v1/embeddings", "POST /v1/rerank"):
        assert fake.count(probe) >= 1, f"T25 never sent {probe}"


@pytest.mark.parametrize("fake", ["bad-chat"], indirect=True)
def test_t25_fails_on_schema_and_still_cleans_up(fake):
    """W02-15 gate: a schema-invalid chat answer fails T25 (exit 1, the probe
    named) and the test consumer is deleted anyway."""
    rc, last, out = _run(SMOKE, "t25", "--base-url", fake.url, "--admin-key-env", "CTX_SELFTEST_ADMIN")
    assert rc == 1 and "chat" in last, f"schema-invalid chat: {rc} {last!r}, want 1 FAIL naming chat\n{out}"
    assert fake.count("model-consumer-delete") == 1, "the test consumer was not deleted after a failure"


@pytest.mark.parametrize("fake", ["house-errors"], indirect=True)
def test_run_requires_the_envelope(fake):
    """W06-19 gate 'rot ohne Envelope' at the live stack: a 401 in the house
    form instead of the OpenAI envelope fails the smoke."""
    rc, last, out = _run(SMOKE, "run", "--base-url", fake.url, "--key-env", "CTX_SELFTEST_KEY")
    assert rc == 1 and "envelope" in last.lower(), f"house-form 401: {rc} {last!r}, want 1 FAIL naming the envelope\n{out}"


@pytest.mark.parametrize("fake,expect,want_rc,needle", [
    ("off", "off", 0, "PASS"),
    ("door-first", "off", 1, "door"),
    ("on", "on", 0, "PASS"),
    ("off", "on", 1, "404"),
    ("proxy-unset", "on", 1, "CTX_TRUSTED_PROXY"),
], indirect=["fake"])
def test_flip_probe(fake, expect, want_rc, needle):
    """W06-20 gate (live half of TestV1FlipProbe): flag off ⇒ anonymous POST
    /v1/chat/completions is 404 not_found (red when the door answers first);
    flag on ⇒ 401 invalid_api_key; 503 proxy_misconfigured names
    CTX_TRUSTED_PROXY."""
    rc, last, out = _run(SMOKE, "flip-probe", "--base-url", fake.url, "--expect", expect)
    assert rc == want_rc and needle in last, f"flip-probe --expect {expect} against {fake.mode}: {rc} {last!r}, want {want_rc} with {needle!r}\n{out}"


@pytest.mark.parametrize("fake,verdict,want_rc", [
    ("proxy-unset", "trusted_proxy_unset", 1),
    ("on", "trusted_proxy_set", 0),
], indirect=["fake"])
def test_measure_proxy(fake, verdict, want_rc):
    """W02-15 / E-01-13: the proxy probe tells an unset CTX_TRUSTED_PROXY (503
    proxy_misconfigured) from a set one (the door's 401)."""
    rc, last, out = _run(MEASURE, "proxy", "--base-url", fake.url)
    got = json.loads(last) if last.startswith("{") else {}
    assert rc == want_rc and got.get("verdict") == verdict, f"proxy probe against {fake.mode}: {rc} {last!r}, want verdict {verdict}\n{out}"


@pytest.mark.parametrize("fake,ok", [("retry-ok", True), ("retry-bad", False)], indirect=["fake"])
def test_measure_retry_after(fake, ok):
    """W02-15: every 429 under the flood carries Retry-After in [1, 30] and
    x-should-retry; the 429 share is recorded."""
    rc, last, out = _run(MEASURE, "retry-after", "--base-url", fake.url, "--key-env", "CTX_SELFTEST_KEY",
                         "--model", "chat", "--requests", "20")
    got = json.loads(last) if last.startswith("{") else {}
    assert got.get("ok") is ok and rc == (0 if ok else 1), f"retry-after against {fake.mode}: {rc} {last!r}, want ok={ok}\n{out}"
    assert 0.3 <= (got.get("share_429") or 0) <= 0.7, f"429 share not recorded: {got}"


@pytest.mark.parametrize("fake", ["heap"], indirect=True)
def test_measure_heap(fake, tmp_path):
    """W02-15: the memory peak under the flood is measured against the
    in-flight budget (peak − base < budget). The fake's memory follows its
    open streams (100 MB + 20 MB each), so 4 concurrent streams lift the
    peak at least 20 MB above the base."""
    mem = tmp_path / "memory.current"
    fake.mem_file = mem
    fake.set_mem(0)
    for budget, ok in ((256 << 20, True), (10 << 20, False)):
        rc, last, out = _run(MEASURE, "heap", "--base-url", fake.url, "--key-env", "CTX_SELFTEST_KEY", "--model", "chat",
                             "--streams", "4", "--consumers-keys-env", "CTX_SELFTEST_KEY", "--mem-file", str(mem),
                             "--budget-bytes", str(budget))
        got = json.loads(last) if last.startswith("{") else {}
        assert got.get("ok") is ok and rc == (0 if ok else 1), f"heap with budget {budget}: {rc} {last!r}, want ok={ok}\n{out}"
        assert got.get("base_bytes") == Fake.MEM_BASE and got.get("peak_bytes", 0) >= Fake.MEM_BASE + Fake.MEM_PER_STREAM, \
            f"heap with budget {budget}: base/peak {got.get('base_bytes')}/{got.get('peak_bytes')} do not follow the flood"
        assert got.get("failed_streams") == 0, f"streams failed under the fake: {got}"


@pytest.mark.parametrize("fake,cut", [("slow-prefill", False), ("cut", True)], indirect=["fake"])
def test_measure_prefill(fake, cut):
    """W02-15: the prefill probe measures the time to the first token
    (≥ the fake's 0.4 s) and reports a stream cut before its first token."""
    rc, last, out = _run(MEASURE, "prefill", "--base-url", fake.url, "--key-env", "CTX_SELFTEST_KEY", "--model", "chat",
                         "--tokens", "2000")
    got = json.loads(last) if last.startswith("{") else {}
    assert got.get("cut_off") is cut and rc == (1 if cut else 0), f"prefill against {fake.mode}: {rc} {last!r}, want cut_off={cut}\n{out}"
    if not cut:
        assert got.get("prefill_s", 0) >= 0.35 and got.get("first_byte_s", 9) < got["prefill_s"], f"prefill times {got}"


@pytest.mark.parametrize("fake,foreign,backends,want_rc,needle", [
    ("on", [{"target": "http://spark:30000", "suspected": False, "suspected_hourly": False}],
     [{"name": "spark-chat", "limits": {"internal_reserved_slots": 4, "abort_settle_ms": {"stream": 500}}}], 0, "PASS"),
    ("on", [{"target": "http://spark:30000", "suspected": True, "suspected_hourly": False}],
     [{"name": "spark-chat", "limits": {"internal_reserved_slots": 4, "abort_settle_ms": {"stream": 500}}}], 1, "foreign load"),
    ("on", [], [{"name": "spark-chat", "limits": {"internal_reserved_slots": 2}}], 1, "internal_reserved_slots"),
    ("on", [], [{"name": "spark-chat", "limits": {"internal_reserved_slots": 4}}], 1, "abort_settle_ms"),
], indirect=["fake"])
def test_preflight(fake, foreign, backends, want_rc, needle):
    """W02-15 checklist items foreign-load and partition: preflight fails on a
    suspected foreign load, a wrong internal reservation or a missing
    abort_settle_ms."""
    fake.foreign_load, fake.backends = foreign, backends
    rc, last, out = _run(SMOKE, "preflight", "--base-url", fake.url, "--admin-key-env", "CTX_SELFTEST_ADMIN",
                         "--partition", "spark-chat=4")
    assert rc == want_rc and needle in last, f"preflight: {rc} {last!r}, want {want_rc} with {needle!r}\n{out}"


def test_checklist_tools_are_implemented():
    """W02-15 gate 'Flip erst bei grüner Checkliste': every probe/measure item
    of checklist.json names a subcommand smoke.py or measure.py implements."""
    items = json.loads((LIVE / "checklist.json").read_text())["items"]
    impl = {}
    for script in (SMOKE, MEASURE):
        rc, _, out = _run(script, "--list")
        impl[script.name] = set(out.split())
    missing = []
    for it in items:
        words = it["tool"].split()
        if words[0] in impl and words[1] not in impl[words[0]]:
            missing.append(f"{it['id']}: {it['tool']}")
    assert not missing, f"checklist items without an implemented tool: {missing}"


def test_testsh_t25_maps_skip():
    """W02-15 gate 'T25 ohne Flag ⇒ SKIP mit Grund (nie PASS)': test.sh has a
    T25 that runs smoke.py t25 and maps exit 77 to skip, never to pass."""
    src = (REPO / "test.sh").read_text()
    m = re.search(r'\nT="T25 [^"]*"\n(.*?)\n(?=# T\d\d |\n# [-=]{3}|\Z)', src, re.S)
    assert m, "test.sh has no T25 block"
    block = m.group(1)
    assert "smoke.py" in block and " t25 " in block + " ", "T25 does not run smoke.py t25"
    assert re.search(r"77\)\s*skip", block), "T25 does not map exit 77 to skip"
    assert not re.search(r"77\)\s*pass", block), "T25 maps exit 77 to pass"


def test_docs_list_every_checklist_id():
    """W06-20: docs/operations.md 'Upgrading to 5.33.0: enabling the
    OpenAI-compatible API' lists every checklist id and CTX_TRUSTED_PROXY."""
    ops = (REPO / "docs" / "operations.md").read_text()
    head = "### Upgrading to 5.33.0: enabling the OpenAI-compatible API"
    assert head in ops, f"docs/operations.md lacks {head!r}"
    sec = ops[ops.index(head):]
    nxt = sec.find("\n### ", 1)
    sec = sec if nxt < 0 else sec[:nxt]
    ids = [it["id"] for it in json.loads((LIVE / "checklist.json").read_text())["items"]]
    missing = [i for i in ids if f"`{i}`" not in sec]
    assert not missing and "CTX_TRUSTED_PROXY" in sec, f"activation section misses checklist ids {missing}"


@pytest.mark.parametrize("fake,want", [("off", "[SKIP] T25 OPENAI_GATEWAY -- gateway.enabled is false"),
                                       ("on", "[OK]   T25 OPENAI_GATEWAY (7 probes"),
                                       ("bad-chat", "[FAIL] T25 OPENAI_GATEWAY -- rc=1 FAIL: chat:")], indirect=["fake"])
def test_testsh_t25_block_runs(fake, want):
    """W02-15 gate end to end: the T25 block of test.sh, run by bash with
    test.sh's own pass/fail/skip helpers against a fake stack, reports SKIP
    with the reason without the flag, OK with it, FAIL on a broken chat."""
    src = (REPO / "test.sh").read_text()
    helpers = re.search(r"\npass\(\) \{.*?\n\}\n\nfail\(\) \{.*?\n\}\n\nskip\(\) \{.*?\n\}\n", src, re.S)
    block = re.search(r'\n(T="T25 [^"]*"\n.*?)\n(?=# T\d\d |\n# [-=]{3}|\Z)', src, re.S)
    assert helpers and block, "test.sh lacks the helpers or the T25 block"
    script = "PASS=0; FAIL=0; SKIP=0\n" + helpers.group(0) + block.group(1) + "\n"
    env = dict(os.environ, WEBHOOK=fake.url, CTX_ADMIN_KEY=ADMIN, SCRIPT_DIR=str(REPO))
    p = subprocess.run(["bash", "-c", 'SCRIPT_DIR="$SCRIPT_DIR"; WEBHOOK="$WEBHOOK"\n' + script],
                       capture_output=True, text=True, timeout=180, env=env)
    out = p.stdout + p.stderr
    assert want in out, f"T25 block against {fake.mode}: want {want!r} in\n{out}"


@pytest.mark.parametrize("fake", ["no-ratelimit"], indirect=True)
def test_run_requires_the_meter_headers(fake):
    """W06-20 live probe (design/06 §7 W06-20 "Chat 200 mit x-ratelimit-*"):
    a served chat without the consumer's x-ratelimit-* headers fails."""
    rc, last, out = _run(SMOKE, "run", "--base-url", fake.url, "--key-env", "CTX_SELFTEST_KEY")
    assert rc == 1 and "x-ratelimit" in last, f"chat without meter headers: {rc} {last!r}, want 1 FAIL naming x-ratelimit\n{out}"
