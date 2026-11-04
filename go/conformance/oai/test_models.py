"""Conformance harness, Python half: the model list (design/02 section 4.4.3, wave W02-05).

internal/oai/pyconformance_test.go serves the model fixture of the Go half
(conformanceModels) under ``CTX_MODELS_BASE_URL``: an app consumer holding
grants on the aliases ``chat`` and ``qwen/x``. The official Python SDK must
list exactly those, retrieve the slash id, tolerate the ``ctx`` extension and
see a not-released alias as ``NotFoundError`` with ``model_not_found``; the
list and the element bodies are validated against the vendored
``ListModelsResponse`` / ``Model`` (testdata, openai-openapi @ c2aecc2f).
"""

import json
import os
import pathlib

import jsonschema
import openai
import pytest

BASE = os.environ["CTX_MODELS_BASE_URL"]
EXCERPT = pathlib.Path(__file__).parent / "testdata" / "openapi-c2aecc2f-list-models-response.json"
RELEASED = ["chat", "qwen/x"]


def _validator(name):
    doc = json.loads(EXCERPT.read_text())
    defs = json.loads(
        json.dumps(doc["components"]["schemas"]).replace('"#/components/schemas/', '"#/$defs/')
    )
    schema = {"$ref": f"#/$defs/{name}", "$defs": defs}
    jsonschema.Draft202012Validator.check_schema(schema)
    return jsonschema.Draft202012Validator(schema)


LIST = _validator("ListModelsResponse")
MODEL = _validator("Model")


@pytest.fixture
def client():
    return openai.OpenAI(base_url=BASE, api_key="ctxm_conformance", max_retries=0, timeout=30)


def test_models_list(client):
    raw = client.models.with_raw_response.list()
    body = json.loads(raw.text)
    problems = [f"list body fails ListModelsResponse: {v.message}" for v in LIST.iter_errors(body)]
    page = raw.parse()
    ids = [m.id for m in page.data]
    if ids != RELEASED:
        problems.append(f"ids {ids}, want {RELEASED}")
    for m in page.data:
        if m.object != "model" or m.owned_by != "ctx" or not isinstance(m.created, int):
            problems.append(f"{m.id}: object {m.object!r} owned_by {m.owned_by!r} created {m.created!r}")
        extra = (m.model_extra or {}).get("ctx")
        if not isinstance(extra, dict) or "kind" not in extra:
            problems.append(f"{m.id}: ctx extension {extra!r} not kept as an extra field")
    assert not problems, "; ".join(problems)


def test_models_retrieve_slash_id(client):
    raw = client.models.with_raw_response.retrieve("qwen/x")
    body = json.loads(raw.text)
    problems = [f"element fails Model: {v.message}" for v in MODEL.iter_errors(body)]
    m = raw.parse()
    if m.id != "qwen/x":
        problems.append(f"id {m.id!r}, want 'qwen/x'")
    assert not problems, "; ".join(problems)


def test_models_retrieve_not_released(client):
    with pytest.raises(openai.NotFoundError) as info:
        client.models.retrieve("embed")
    assert info.value.code == "model_not_found", info.value.code
