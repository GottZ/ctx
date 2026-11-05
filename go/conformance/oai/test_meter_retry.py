"""Conformance harness, Python half: the meter's 429 (wave W06-10, design/06 section 4.4).

internal/oai/pyconformance_test.go serves the chat route of the Go harness
(newChatRig) over the real per-consumer meter (internal/keyquota, real clock,
12 requests per minute) under ``CTX_METER_BASE_URL``. The test exhausts the
request bucket with plain HTTP, checks the refusal (429 rate_limit_exceeded,
``x-should-retry: true``, ``Retry-After`` and ``retry-after-ms``), then lets
the official Python SDK send the same request: the SDK must retry the 429 and
wait at least the ``retry-after-ms`` the meter answered before it retries,
and the retry is served.
"""

import json
import os
import time
import urllib.error
import urllib.request

import openai

BASE = os.environ["CTX_METER_BASE_URL"]
BODY = {"model": "chat", "messages": [{"role": "user", "content": "hi"}]}
KEY = "ctxm_conformance"


def _post():
    req = urllib.request.Request(
        f"{BASE}v1/chat/completions",
        data=json.dumps(BODY).encode(),
        headers={"Authorization": f"Bearer {KEY}", "Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, r.headers, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read()


def test_meter_429_sdk_waits_retry_after():
    refused = None
    for _ in range(100):
        status, headers, body = _post()
        if status == 429:
            refused = (headers, body)
            break
        assert status == 200, body
    assert refused is not None, "the meter never refused within 100 requests"
    headers, body = refused
    err = json.loads(body)["error"]
    assert (err["type"], err["code"]) == ("rate_limit_error", "rate_limit_exceeded"), err
    assert headers.get("x-should-retry") == "true", dict(headers)
    assert int(headers["Retry-After"]) >= 1, dict(headers)
    assert int(headers["retry-after-ms"]) >= 1, dict(headers)

    events = []
    hooks = {
        "request": [lambda req: events.append(("request", time.monotonic(), None))],
        "response": [lambda resp: events.append(("response", time.monotonic(), resp))],
    }
    client = openai.OpenAI(
        base_url=f"{BASE}v1/",
        api_key=KEY,
        max_retries=2,
        timeout=30,
        http_client=openai.DefaultHttpxClient(event_hooks=hooks),
    )
    c = client.chat.completions.create(model=BODY["model"], messages=BODY["messages"])
    assert c.choices[0].message.content == "hello from the engine"

    statuses = [e[2].status_code for e in events if e[0] == "response"]
    assert statuses and statuses[0] == 429 and statuses[-1] == 200, statuses
    i = next(i for i, e in enumerate(events) if e[0] == "response" and e[2].status_code == 429)
    wait_ms = int(events[i][2].headers["retry-after-ms"])
    assert events[i][2].headers.get("x-should-retry") == "true"
    retry_at = next(e[1] for e in events[i + 1 :] if e[0] == "request")
    waited = retry_at - events[i][1]
    assert waited >= wait_ms / 1000, f"the SDK retried after {waited:.3f}s, the meter said retry-after-ms {wait_ms}"
