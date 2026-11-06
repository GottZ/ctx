"""Conformance harness, Python half: the meter's L1 refusal (wave W06-11, design/06 section 4.4).

internal/oai/pyconformance_test.go serves the chat route of the Go harness
(newChatRig) over the real per-consumer meter with a concurrency limit of 1
under ``CTX_CONCURRENCY_BASE_URL``; its engine holds a request whose message
is "hold" until ``POST /release`` and answers ``GET /held`` once that request
reached it. The test takes the consumer's one slot with such a request, waits
until the engine has it, checks the refusal of a second one with plain HTTP (429
concurrency_limit_exceeded, ``Retry-After: 1``, ``retry-after-ms: 1000``,
``x-should-retry: true``), then lets the official Python SDK send a request:
its first try meets the full slot, the response hook releases the held
request, and the SDK must wait at least the one second the refusal asked for
before its retry, which is served. Whatever fails on the way, the held
request is released and its thread joined before the test ends.
"""

import json
import os
import threading
import time
import urllib.error
import urllib.request

import openai

BASE = os.environ["CTX_CONCURRENCY_BASE_URL"]
KEY = "ctxm_conformance"


def _post(path, body):
    req = urllib.request.Request(
        f"{BASE}{path}",
        data=json.dumps(body).encode(),
        headers={"Authorization": f"Bearer {KEY}", "Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status, r.headers, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read()


def _held():
    try:
        with urllib.request.urlopen(f"{BASE}held", timeout=60) as r:
            return r.status
    except urllib.error.HTTPError as e:
        return e.code


def _chat(content):
    return _post("v1/chat/completions", {"model": "chat", "messages": [{"role": "user", "content": content}]})


def test_concurrency_429_sdk_waits_one_second():
    held = {}
    holder = threading.Thread(target=lambda: held.update(answer=_chat("hold")))
    holder.start()
    try:
        events = _refused_then_served()
    finally:
        _post("release", {})
        holder.join(timeout=60)
    assert held.get("answer", (None,))[0] == 200, held

    statuses = [e[2].status_code for e in events if e[0] == "response"]
    assert statuses and statuses[0] == 429 and statuses[-1] == 200, statuses
    i = next(i for i, e in enumerate(events) if e[0] == "response" and e[2].status_code == 429)
    assert events[i][2].headers["retry-after-ms"] == "1000"
    assert events[i][2].headers.get("x-should-retry") == "true"
    retry_at = next(e[1] for e in events[i + 1 :] if e[0] == "request")
    waited = retry_at - events[i][1]
    assert waited >= 1.0, f"the SDK retried after {waited:.3f}s, the refusal said retry-after-ms 1000"


def _refused_then_served():
    """The part between taking the slot and releasing it; returns the SDK's request and response events."""
    assert _held() == 204, "the held request never reached the engine"
    status, headers, body = _chat("hi")
    assert status == 429, f"a second request beside the held one answers {status}: {body!r}"
    err = json.loads(body)["error"]
    assert (err["type"], err["code"]) == ("rate_limit_error", "concurrency_limit_exceeded"), err
    assert headers["Retry-After"] == "1", dict(headers)
    assert headers["retry-after-ms"] == "1000", dict(headers)
    assert headers.get("x-should-retry") == "true", dict(headers)

    events = []

    def on_response(resp):
        events.append(("response", time.monotonic(), resp))
        if resp.status_code == 429:
            _post("release", {})

    hooks = {
        "request": [lambda req: events.append(("request", time.monotonic(), None))],
        "response": [on_response],
    }
    client = openai.OpenAI(
        base_url=f"{BASE}v1/",
        api_key=KEY,
        max_retries=2,
        timeout=30,
        http_client=openai.DefaultHttpxClient(event_hooks=hooks),
    )
    c = client.chat.completions.create(model="chat", messages=[{"role": "user", "content": "hi"}])
    assert c.choices[0].message.content == "hello from the engine"
    return events
