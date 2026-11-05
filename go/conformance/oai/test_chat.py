"""Conformance harness, Python half: POST /v1/chat/completions non-stream (design/02 section 4.6, wave W02-07, gate G1).

internal/oai/pyconformance_test.go serves the chat route of the Go harness
(newChatRig) once per answer shape of the live chat chain under
``CTX_CHAT_BASE_URL``: ``<base><shape>/v1/`` with ``shape`` in ``sglang``,
``llamacpp``, ``openrouter``. The official Python SDK must parse each answer
as a chat completion whose ``id`` is ``chatcmpl-<server-side UUIDv7>`` (never
the client's ``X-Request-ID``), whose ``model`` is the alias name, whose usage
counts are positive and whose message carries no reasoning trace; the wire
body is validated against the vendored ``CreateChatCompletionResponse``
(testdata, openai-openapi @ c2aecc2f).
"""

import json
import os
import pathlib
import uuid

import jsonschema
import openai
import pytest

BASE = os.environ["CTX_CHAT_BASE_URL"]
EXCERPT = pathlib.Path(__file__).parent / "testdata" / "openapi-c2aecc2f-create-chat-completion-response.json"
SHAPES = ["sglang", "llamacpp", "openrouter"]
CLIENT_REQUEST_ID = "deadbeef"


def _validator(name):
    doc = json.loads(EXCERPT.read_text())
    defs = json.loads(
        json.dumps(doc["components"]["schemas"]).replace('"#/components/schemas/', '"#/$defs/')
    )
    schema = {"$ref": f"#/$defs/{name}", "$defs": defs}
    jsonschema.Draft202012Validator.check_schema(schema)
    return jsonschema.Draft202012Validator(schema)


CHAT = _validator("CreateChatCompletionResponse")


@pytest.mark.parametrize("shape", SHAPES)
def test_chat_completion(shape):
    client = openai.OpenAI(base_url=f"{BASE}{shape}/v1/", api_key="ctxm_conformance", max_retries=0, timeout=30)
    raw = client.chat.completions.with_raw_response.create(
        model="chat",
        messages=[{"role": "user", "content": "hi"}],
        extra_headers={"X-Request-ID": CLIENT_REQUEST_ID},
    )
    body = json.loads(raw.text)
    problems = [f"wire body fails CreateChatCompletionResponse: {v.message}" for v in CHAT.iter_errors(body)]
    c = raw.parse()
    prefix, _, suffix = c.id.partition("chatcmpl-")
    if prefix or not suffix:
        problems.append(f"id {c.id!r} lacks the chatcmpl- prefix")
    elif CLIENT_REQUEST_ID in c.id:
        problems.append(f"id {c.id!r} carries the client's X-Request-ID")
    else:
        try:
            if uuid.UUID(suffix).version != 7:
                problems.append(f"id {c.id!r}: suffix is no UUIDv7")
        except ValueError:
            problems.append(f"id {c.id!r}: suffix is no UUID")
    if c.model != "chat":
        problems.append(f"model {c.model!r}, want the alias name 'chat'")
    if c.object != "chat.completion":
        problems.append(f"object {c.object!r}")
    u = c.usage
    if u is None or u.prompt_tokens <= 0 or u.completion_tokens <= 0 or u.total_tokens <= 0:
        problems.append(f"usage {u!r}, want every count > 0")
    if len(c.choices) != 1 or c.choices[0].message.content != "hello from the engine":
        problems.append(f"choices {c.choices!r}")
    else:
        extra = c.choices[0].message.model_extra or {}
        if "reasoning_content" in extra:
            problems.append("the message carries reasoning_content (reasoning-off alias)")
    if "reasoning_content" in raw.text or "THINK-CANARY" in raw.text:
        problems.append("the wire body carries the reasoning trace")
    assert not problems, "; ".join(problems)
