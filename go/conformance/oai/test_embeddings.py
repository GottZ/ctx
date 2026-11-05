"""Conformance harness, Python half: POST /v1/embeddings (design/02 section 4.8, wave W02-09, gate G1).

internal/oai/pyconformance_test.go serves the embeddings route of the Go
harness (newEmbedRig over a fake engine whose vectors are a function of the
text) under ``CTX_EMBED_BASE_URL``: ``<base>v1/``. The official Python SDK
sends ``encoding_format: base64`` when the caller names none and decodes the
answer itself; it must get the same float32 vectors as the explicit
``float`` request, bit for bit, at their index, for the alias ``emb4k``
(native 4096, matryoshka from 256). Every wire body is validated against the
vendored ``CreateEmbeddingResponse`` (testdata, openai-openapi @ c2aecc2f).
Without the route the SDK call is a 404 (gate G1 red).
"""

import json
import os
import pathlib
import struct

import jsonschema
import openai

BASE = os.environ["CTX_EMBED_BASE_URL"]
EXCERPT = pathlib.Path(__file__).parent / "testdata" / "openapi-c2aecc2f-create-embedding-response.json"
INPUTS = ["alpha", "beta", "gamma"]


def _validator(name):
    doc = json.loads(EXCERPT.read_text())
    defs = json.loads(
        json.dumps(doc["components"]["schemas"]).replace('"#/components/schemas/', '"#/$defs/')
    )
    schema = {"$ref": f"#/$defs/{name}", "$defs": defs}
    jsonschema.Draft202012Validator.check_schema(schema)
    return jsonschema.Draft202012Validator(schema)


EMBEDDING = _validator("CreateEmbeddingResponse")


def _client():
    return openai.OpenAI(base_url=f"{BASE}v1/", api_key="ctxm_conformance", max_retries=0, timeout=30)


def _f32(xs):
    # the float32 bit patterns of a vector
    return [struct.unpack("<I", struct.pack("<f", x))[0] for x in xs]


def _check_wire(raw, problems, what):
    body = json.loads(raw.text)
    problems.extend(f"{what}: wire body fails CreateEmbeddingResponse: {v.message}" for v in EMBEDDING.iter_errors(body))
    return body


def test_embeddings_default_is_base64_and_bit_identical_to_float():
    problems = []
    client = _client()
    raw_default = client.embeddings.with_raw_response.create(model="emb4k", input=INPUTS)
    wire_default = _check_wire(raw_default, problems, "default")
    if not isinstance(wire_default["data"][0]["embedding"], str):
        problems.append("the SDK default did not ask for base64 (embedding is no string on the wire)")
    default = raw_default.parse()
    raw_float = client.embeddings.with_raw_response.create(model="emb4k", input=INPUTS, encoding_format="float")
    _check_wire(raw_float, problems, "float")
    flt = raw_float.parse()
    if default.model != "emb4k" or flt.model != "emb4k":
        problems.append(f"model {default.model!r}/{flt.model!r}, want the alias name 'emb4k'")
    if len(default.data) != 3 or len(flt.data) != 3:
        problems.append(f"data {len(default.data)}/{len(flt.data)}, want 3")
    for i, (d, f) in enumerate(zip(default.data, flt.data)):
        if d.index != i or f.index != i:
            problems.append(f"index {d.index}/{f.index} at position {i}")
        if len(d.embedding) != 4096:
            problems.append(f"data[{i}] has {len(d.embedding)} dimensions, want the native 4096")
        if _f32(d.embedding) != _f32(f.embedding):
            problems.append(f"data[{i}]: base64 and float differ")
    if default.usage.prompt_tokens <= 0 or default.usage.total_tokens != default.usage.prompt_tokens:
        problems.append(f"usage {default.usage!r}")
    assert not problems, "; ".join(problems)


def test_embeddings_dimensions():
    problems = []
    client = _client()
    raw = client.embeddings.with_raw_response.create(model="emb4k", input="alpha", dimensions=256)
    _check_wire(raw, problems, "dimensions")
    r = raw.parse()
    v = r.data[0].embedding
    if len(v) != 256:
        problems.append(f"{len(v)} dimensions, want 256")
    norm = sum(x * x for x in v) ** 0.5
    if abs(norm - 1) > 1e-6:
        problems.append(f"norm {norm}, want 1")
    assert not problems, "; ".join(problems)


def test_embeddings_refusals_parse_as_sdk_errors():
    client = _client()
    try:
        client.embeddings.create(model="emb4k", input=[[1, 2, 3]])
    except openai.BadRequestError as e:
        assert e.code == "unsupported_value", e.code
    else:
        raise AssertionError("a token array was accepted")
    try:
        client.embeddings.create(model="emb4k", input="alpha", dimensions=8192)
    except openai.BadRequestError as e:
        assert e.code == "unsupported_value", e.code
    else:
        raise AssertionError("dimensions above the native dimension were accepted")
