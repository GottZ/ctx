"""Conformance harness, Python half: POST /v1/chat/completions stream (design/02 sections 4.6, 4.7, 4.10.3, wave W02-08, gates G1, G3, G5, G7).

internal/oai/pyconformance_test.go serves the stream route of the Go harness
under ``CTX_CHAT_STREAM_BASE_URL``, each rig at ``<base><name>/v1/``:

* ``sglang``, ``llamacpp``, ``openrouter`` - the engine streams three deltas
  and a usage chunk (G1, with and without ``stream_options.include_usage``);
* ``error`` - the engine breaks off after its first chunk with an error
  object (G7);
* ``busy`` - the one external slot is held by another consumer, wait budget
  100 ms (G3);
* ``keepalive`` - the engine is silent for 3 x grace_eff (15 s) after the
  admission, keepalive every second (G5);
* ``reasoning-role`` - SGLang sends the assistant role only in its first,
  reasoning-only chunk, which the reasoning-off alias drops (W02-08
  hardening 5).

The SDK's HTTP transport is tapped: it counts the requests and keeps every
byte of the response body with its arrival time, so the wire - every data
payload, not only the last - is checked against the vendored
``CreateChatCompletionStreamResponse`` (testdata, openai-openapi @ c2aecc2f)
and the SSE framing against the relay contract: data and comment lines only,
one id/created/model in every chunk, ``[DONE]`` exactly once and last, an
error frame never followed by ``[DONE]``.
"""

import json
import os
import pathlib
import time
import uuid

import httpx2
import jsonschema
import openai
import pytest

BASE = os.environ["CTX_CHAT_STREAM_BASE_URL"]
TESTDATA = pathlib.Path(__file__).parent / "testdata"
SHAPES = ["sglang", "llamacpp", "openrouter"]
CLIENT_REQUEST_ID = "deadbeef"
STREAM_TEXT = "hello from the engine"
MESSAGES = [{"role": "user", "content": "hi"}]
# The keepalive rig's engine silence (3 x grace_eff) and a read timeout below
# it: without keepalives the read would time out.
SILENCE_S = 15.0
READ_TIMEOUT_S = 8.0


def _validator(excerpt, name):
    doc = json.loads((TESTDATA / excerpt).read_text())
    defs = json.loads(
        json.dumps(doc["components"]["schemas"]).replace('"#/components/schemas/', '"#/$defs/')
    )
    schema = {"$ref": f"#/$defs/{name}", "$defs": defs}
    jsonschema.Draft202012Validator.check_schema(schema)
    return jsonschema.Draft202012Validator(schema)


CHUNK = _validator("openapi-c2aecc2f-create-chat-completion-stream-response.json", "CreateChatCompletionStreamResponse")
ERROR = _validator("openapi-c2aecc2f-error-response.json", "ErrorResponse")


class Tap(httpx2.BaseTransport):
    """The SDK's transport: counts the requests and reads each response body
    to its end, keeping every part with its arrival time (``parts``) - the
    bytes after ``[DONE]`` or an error frame belong to the wire too. The read
    timeout of the request still applies to every network read."""

    def __init__(self):
        self._inner = httpx2.HTTPTransport()
        self.requests = 0
        self.sent = 0.0
        self.parts = []

    def handle_request(self, request):
        self.requests += 1
        self.sent = time.monotonic()
        resp = self._inner.handle_request(request)
        self.parts = []
        try:
            for part in resp.stream:
                self.parts.append((time.monotonic(), part))
        finally:
            resp.stream.close()
        return httpx2.Response(
            status_code=resp.status_code,
            headers=resp.headers,
            stream=httpx2.ByteStream(self.wire),
            extensions=resp.extensions,
            request=request,
        )

    @property
    def wire(self):
        return b"".join(p for _, p in self.parts)

    def close(self):
        self._inner.close()


def _client(name, tap, retries=0, timeout=30.0):
    return openai.OpenAI(
        base_url=f"{BASE}{name}/v1/",
        api_key="ctxm_conformance",
        max_retries=retries,
        timeout=timeout,
        http_client=httpx2.Client(transport=tap),
    )


def _events(raw):
    """Parses an SSE body into (kind, value) events: data, comment, other."""
    out, data = [], []
    for line in raw.decode("utf-8").split("\n"):
        line = line.rstrip("\r")
        if line == "":
            if data:
                out.append(("data", "\n".join(data)))
                data = []
        elif line.startswith("data:"):
            value = line[len("data:"):]
            data.append(value[1:] if value.startswith(" ") else value)
        elif line.startswith(":"):
            out.append(("comment", line[1:].strip()))
        else:
            out.append(("other", line))
    if data:
        out.append(("data", "\n".join(data)))
    return out


def _wire_chunks(raw, problems):
    """Checks the framing and every chunk payload; returns (chunks, datas)."""
    events = _events(raw)
    for kind, value in events:
        if kind == "other":
            problems.append(f"wire line {value!r} - the relay writes data: and comments only")
    datas = [v for k, v in events if k == "data"]
    chunks, first = [], None
    for d in datas:
        if d == "[DONE]":
            continue
        try:
            m = json.loads(d)
        except ValueError as e:
            problems.append(f"wire chunk is no JSON: {e} ({d!r})")
            continue
        if isinstance(m, dict) and "error" in m:
            continue
        problems.extend(f"wire chunk fails CreateChatCompletionStreamResponse: {v.message} ({d})" for v in CHUNK.iter_errors(m))
        if first is None:
            first = m
        for k in ("id", "created", "model", "object"):
            if m.get(k) != first.get(k):
                problems.append(f"wire chunk {k} = {m.get(k)!r}, the first chunk had {first.get(k)!r}")
        chunks.append(m)
    return chunks, datas, events


def _check_id(cid, problems):
    prefix, _, suffix = cid.partition("chatcmpl-")
    if prefix or not suffix:
        problems.append(f"id {cid!r} lacks the chatcmpl- prefix")
    elif CLIENT_REQUEST_ID in cid:
        problems.append(f"id {cid!r} carries the client's X-Request-ID")
    else:
        try:
            if uuid.UUID(suffix).version != 7:
                problems.append(f"id {cid!r}: suffix is no UUIDv7")
        except ValueError:
            problems.append(f"id {cid!r}: suffix is no UUID")


@pytest.mark.parametrize("include_usage", [True, False], ids=["include_usage", "no_usage"])
@pytest.mark.parametrize("shape", SHAPES)
def test_chat_stream(shape, include_usage):
    """G1: the SDK iterates a conformant chunk stream of every engine shape."""
    tap = Tap()
    client = _client(shape, tap)
    extra = {"stream_options": {"include_usage": True}} if include_usage else {}
    stream = client.chat.completions.create(
        model="chat", messages=MESSAGES, stream=True, extra_headers={"X-Request-ID": CLIENT_REQUEST_ID}, **extra
    )
    got = list(stream)
    problems = []
    if not got:
        pytest.fail(f"the SDK read no chunk; wire {tap.wire!r}")
    # The SDK's typed view.
    first = got[0]
    for i, c in enumerate(got):
        if (c.id, c.created, c.model) != (first.id, first.created, first.model):
            problems.append(f"chunk {i}: id/created/model {c.id}/{c.created}/{c.model} differ from the first chunk")
        if c.object != "chat.completion.chunk":
            problems.append(f"chunk {i}: object {c.object!r}")
    _check_id(first.id, problems)
    if first.model != "chat":
        problems.append(f"model {first.model!r}, want the alias name 'chat'")
    content = "".join(ch.delta.content or "" for c in got for ch in c.choices)
    if content != STREAM_TEXT:
        problems.append(f"content {content!r}, want {STREAM_TEXT!r}")
    with_usage = [i for i, c in enumerate(got) if c.usage is not None]
    if include_usage:
        if with_usage != [len(got) - 1]:
            problems.append(f"usage chunks at {with_usage} of {len(got)}, want exactly the last one")
        else:
            last = got[-1]
            u = last.usage
            if last.choices or (u.prompt_tokens, u.completion_tokens, u.total_tokens) != (11, 3, 14):
                problems.append(f"usage chunk choices {last.choices!r} usage {u!r}, want [] and 11/3/14")
    else:
        if with_usage:
            problems.append(f"usage chunks at {with_usage} without include_usage, want none")
        for i, c in enumerate(got):
            if not c.choices:
                problems.append(f"chunk {i} has no choices - client code reading choices[0] breaks")
    # The wire.
    chunks, datas, _ = _wire_chunks(tap.wire, problems)
    if datas.count("[DONE]") != 1 or not datas or datas[-1] != "[DONE]":
        problems.append(f"[DONE] {datas.count('[DONE]')} times on the wire (last data {datas[-1:]!r}), want exactly once and last")
    if len(chunks) != len(got):
        problems.append(f"{len(chunks)} chunks on the wire, the SDK read {len(got)}")
    if not include_usage and any("usage" in m for m in chunks):
        problems.append("a wire chunk carries a usage key without include_usage")
    if chunks and chunks[0].get("id") != first.id:
        problems.append(f"wire id {chunks[0].get('id')!r}, the SDK read {first.id!r}")
    if b"reasoning_content" in tap.wire or b"THINK-CANARY" in tap.wire:
        problems.append("the wire carries a reasoning trace")
    if tap.requests != 1:
        problems.append(f"{tap.requests} requests, want 1")
    assert not problems, "; ".join(problems)


def test_chat_stream_busy_is_retried_429():
    """G3: a saturated route refuses a stream with a real 429 before any byte;
    the SDK retries it (max_retries=2: at least one retry)."""
    tap = Tap()
    client = _client("busy", tap, retries=2)
    with pytest.raises(openai.RateLimitError) as ei:
        client.chat.completions.create(model="chat", messages=MESSAGES, stream=True)
    e = ei.value
    problems = []
    if e.status_code != 429 or e.code != "server_busy":
        problems.append(f"{e.status_code} {e.code!r}, want 429 'server_busy'")
    h = e.response.headers
    for name in ("Retry-After", "retry-after-ms"):
        v = h.get(name)
        if v is None or not v.isdigit() or int(v) < 1:
            problems.append(f"{name} {v!r}, want a positive integer")
    if h.get("content-type", "").startswith("text/event-stream"):
        problems.append(f"content-type {h.get('content-type')!r} - a byte left before the admission")
    try:
        body = json.loads(tap.wire)
        problems.extend(f"refusal body fails ErrorResponse: {v.message}" for v in ERROR.iter_errors(body))
    except ValueError as err:
        problems.append(f"refusal body is not JSON: {err} ({tap.wire!r})")
    if tap.requests < 2:
        problems.append(f"the SDK sent {tap.requests} request(s), want at least 2 (one retry of the 429)")
    assert not problems, "; ".join(problems)


def test_chat_stream_keepalive_through_silence():
    """G5: the engine is silent longer than the grace after the admission -
    the route commits, keepalive comments keep the connection (read timeout
    8 s < 15 s silence), and the client's ``chunk.choices[0]`` never breaks."""
    tap = Tap()
    client = _client("keepalive", tap, timeout=httpx2.Timeout(60.0, read=READ_TIMEOUT_S))
    stream = client.chat.completions.create(model="chat", messages=MESSAGES, stream=True)
    content = ""
    n = 0
    for chunk in stream:
        content += chunk.choices[0].delta.content or ""
        n += 1
    problems = []
    if (n, content) != (3, STREAM_TEXT):
        problems.append(f"{n} chunks with {content!r}, want 3 with {STREAM_TEXT!r}")
    _, datas, events = _wire_chunks(tap.wire, problems)
    before = []
    for kind, value in events:
        if kind == "data":
            break
        if kind == "comment":
            before.append(value)
    if len(before) < 2 or any(c != "keepalive" for c in before):
        problems.append(f"comments before the first data frame {before!r}, want at least two 'keepalive'")
    if datas.count("[DONE]") != 1 or datas[-1] != "[DONE]":
        problems.append(f"[DONE] {datas.count('[DONE]')} times, want exactly once and last")
    if tap.parts:
        first_byte = tap.parts[0][0] - tap.sent
        gaps = [b[0] - a[0] for a, b in zip(tap.parts, tap.parts[1:])]
        elapsed = tap.parts[-1][0] - tap.sent
        if elapsed < SILENCE_S:
            problems.append(f"the stream ended after {elapsed:.1f} s, before the engine's {SILENCE_S} s silence")
        if first_byte >= READ_TIMEOUT_S or (gaps and max(gaps) >= READ_TIMEOUT_S):
            problems.append(f"first byte after {first_byte:.1f} s, largest gap {max(gaps or [0]):.1f} s, want both < {READ_TIMEOUT_S} s")
    assert not problems, "; ".join(problems)


def test_chat_stream_helper_keeps_the_role():
    """W02-08 hardening 5: the engine sends the assistant role only in its
    reasoning chunk, which the reasoning-off alias drops; the SDK's stream
    helper accumulates the chunks into a completion whose message needs the
    role - the rewrite carries it into the first relayed chunk, once."""
    tap = Tap()
    client = _client("reasoning-role", tap)
    with client.chat.completions.stream(model="chat", messages=MESSAGES) as stream:
        for _ in stream:
            pass
        final = stream.get_final_completion()
    problems = []
    message = final.choices[0].message
    role = getattr(message, "role", None)
    if role != "assistant":
        problems.append(f"final message role {role!r}, want 'assistant' - the accumulated stream lost the role")
    if message.content != STREAM_TEXT:
        problems.append(f"final content {message.content!r}, want {STREAM_TEXT!r}")
    chunks, datas, _ = _wire_chunks(tap.wire, problems)
    roles = [(i, ch["delta"].get("role")) for i, m in enumerate(chunks) for ch in m.get("choices", []) if "role" in ch["delta"]]
    if roles != [(0, "assistant")]:
        problems.append(f"roles on the wire {roles!r}, want [(0, 'assistant')] - once, in the first chunk")
    if datas.count("[DONE]") != 1 or datas[-1] != "[DONE]":
        problems.append(f"[DONE] {datas.count('[DONE]')} times, want exactly once and last")
    if b"reasoning_content" in tap.wire or b"THINK-CANARY" in tap.wire:
        problems.append("the wire carries a reasoning trace")
    assert not problems, "; ".join(problems)


def test_chat_stream_error_frame():
    """G7: the engine breaks off mid-stream - after the first chunk the SDK
    raises ``openai.APIError`` (in-band, no status error: the stream was
    committed) with the route's upstream_... code; no [DONE] on the wire."""
    tap = Tap()
    client = _client("error", tap)
    stream = client.chat.completions.create(model="chat", messages=MESSAGES, stream=True)
    got = []
    with pytest.raises(openai.APIError) as ei:
        for chunk in stream:
            got.append(chunk.choices[0].delta.content)
    e = ei.value
    problems = []
    if isinstance(e, openai.APIStatusError):
        problems.append(f"{type(e).__name__} {e.status_code} - a status error, want the in-band error of a committed stream")
    if got != ["hello"]:
        problems.append(f"chunks before the error {got!r}, want ['hello']")
    if not (e.code or "").startswith("upstream_"):
        problems.append(f"error code {e.code!r}, want the route's upstream_... code")
    _, datas, _ = _wire_chunks(tap.wire, problems)
    if "[DONE]" in datas:
        problems.append("[DONE] after the error frame")
    if not datas:
        problems.append("no data on the wire")
    else:
        try:
            frame = json.loads(datas[-1])
        except ValueError:
            frame = None
        if not isinstance(frame, dict) or "error" not in frame:
            problems.append(f"last data frame {datas[-1]!r}, want the error frame")
        else:
            problems.extend(f"error frame fails ErrorResponse: {v.message}" for v in ERROR.iter_errors(frame))
            if frame["error"].get("code") != e.code:
                problems.append(f"error frame code {frame['error'].get('code')!r}, the SDK raised {e.code!r}")
    assert not problems, "; ".join(problems)
