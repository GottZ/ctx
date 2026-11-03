"""Conformance harness, Python half (design/02 section 7, wave W02-02).

The official OpenAI Python SDK talks to the Go fake started by
internal/oai/pyconformance_test.go (build tag ``pyconformance``). That test
runs:

    uv run --no-project --with openai==3.42.1 --with jsonschema==4.26.0 \
        --with pytest==9.2.0 pytest -p no:cacheprovider conformance/oai

with ``OPENAI_BASE_URL`` set to the fake's root. Every case is served under
``<root>/c/<case>/``; ``GET <root>/_hits/<case>`` returns how many requests the
fake saw, ``POST <root>/_reset`` clears the counters.

Each case pins what a Python client sees: the exception class the SDK maps the
status to, the parsed ``code``/``type``, the retry count (``x-should-retry:
false`` must stop the SDK's own retries), the headers, and the body against the
vendored ``ErrorResponse`` schema (testdata, openai-openapi @ c2aecc2f).
"""

import json
import os
import pathlib
import time
import urllib.request

import jsonschema
import openai
import pytest

ROOT = os.environ["OPENAI_BASE_URL"].rstrip("/")
EXCERPT = pathlib.Path(__file__).parent / "testdata" / "openapi-c2aecc2f-error-response.json"


def _error_validator():
    doc = json.loads(EXCERPT.read_text())
    # Same mechanical move as the Go loader: definitions under $defs, refs
    # rewritten; the schema values stay the vendored ones.
    defs = json.loads(
        json.dumps(doc["components"]["schemas"]).replace('"#/components/schemas/', '"#/$defs/')
    )
    schema = {"$ref": "#/$defs/ErrorResponse", "$defs": defs}
    jsonschema.Draft202012Validator.check_schema(schema)
    return jsonschema.Draft202012Validator(schema)


VALIDATOR = _error_validator()

# (case, exception class, status, type, code, x-should-retry, max_retries,
#  expected requests at the fake, extra headers)
CASES = [
    ("not_found", openai.NotFoundError, 404, "invalid_request_error", "not_found", "false", 2, 1, {}),
    ("method_not_allowed", openai.APIStatusError, 405, "invalid_request_error", "method_not_allowed", "false", 2, 1,
     {"allow": "POST"}),
    ("invalid_api_key", openai.AuthenticationError, 401, "invalid_request_error", "invalid_api_key", "false", 2, 1,
     {"www-authenticate": 'Bearer realm="ctx"'}),
    ("request_too_large", openai.APIStatusError, 413, "invalid_request_error", "request_too_large", "false", 2, 1, {}),
    ("rate_limit_no_retry", openai.RateLimitError, 429, "rate_limit_error", "rate_limit_exceeded", "false", 2, 1, {}),
    ("internal_error", openai.InternalServerError, 503, "service_unavailable_error", "internal_error", "false", 2, 1,
     {}),
    ("server_overloaded", openai.InternalServerError, 503, "service_unavailable_error", "server_overloaded", "true", 1,
     2, {"retry-after": "1"}),
]


def _hits(case):
    with urllib.request.urlopen(f"{ROOT}/_hits/{case}", timeout=10) as r:
        return json.load(r)["hits"]


def _reset():
    req = urllib.request.Request(f"{ROOT}/_reset", method="POST")
    with urllib.request.urlopen(req, timeout=10):
        pass


@pytest.mark.parametrize(
    "case,exc,status,etype,code,should_retry,max_retries,want_hits,headers",
    CASES,
    ids=[c[0] for c in CASES],
)
def test_error_case(case, exc, status, etype, code, should_retry, max_retries, want_hits, headers):
    _reset()
    client = openai.OpenAI(base_url=f"{ROOT}/c/{case}/", api_key="sk-conformance", max_retries=max_retries,
                           timeout=30)
    start = time.monotonic()
    with pytest.raises(openai.APIStatusError) as info:
        client.models.list()
    elapsed = time.monotonic() - start
    err = info.value
    problems = []
    # Exact class: 405/413 must stay the plain APIStatusError, 404 the
    # NotFoundError and so on — a subclass check would accept any of them.
    if type(err) is not exc:
        problems.append(f"exception {type(err).__name__}, want {exc.__name__}")
    if err.status_code != status:
        problems.append(f"status {err.status_code}, want {status}")
    if err.code != code:
        problems.append(f"SDK parsed code {err.code!r}, want {code!r}")
    if err.type != etype:
        problems.append(f"SDK parsed type {err.type!r}, want {etype!r}")
    hits = _hits(case)
    if hits != want_hits:
        problems.append(f"fake saw {hits} request(s), want {want_hits} (x-should-retry {should_retry})")
    if want_hits > 1 and headers.get("retry-after") == "1" and elapsed < 0.9:
        problems.append(f"retry after {elapsed:.2f}s, want >= 1 s (Retry-After: 1 not honoured)")
    got = err.response.headers.get("x-should-retry")
    if got != should_retry:
        problems.append(f"x-should-retry {got!r}, want {should_retry!r}")
    if not err.response.headers.get("content-type", "").startswith("application/json"):
        problems.append(f"content-type {err.response.headers.get('content-type')!r}, want application/json")
    for k, want in headers.items():
        if err.response.headers.get(k) != want:
            problems.append(f"{k} {err.response.headers.get(k)!r}, want {want!r}")
    try:
        body = err.response.json()
    except ValueError as e:
        problems.append(f"wire body is not JSON: {e} ({err.response.text!r})")
    else:
        for v in VALIDATOR.iter_errors(body):
            problems.append(f"wire body fails ErrorResponse: {v.message}")
    assert not problems, "; ".join(problems)
