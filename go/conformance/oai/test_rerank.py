"""Conformance harness, Python half: POST /v2/rerank (Cohere v2) and POST /v1/rerank (Jina) (design/02 section 4.9, wave W02-10, gate G1).

internal/oai/pyconformance_test.go serves both rerank routes of the Go
harness (conformanceRerank: newRerankRig over a fake cross-encoder that
answers the logits of the table below) under ``CTX_RERANK_BASE_URL``. The
official Cohere SDK (``cohere.ClientV2``, which appends ``v2/rerank`` to its
base URL itself) must parse the Cohere v2 answer: sorted descending, every
relevance score the sigmoid of the document's logit in [0, 1], the indices
of the fixture, no document echo by default. The SDK builds its models
without validation (``construct_type``), so a parse alone proves nothing:
every type and value is checked here. The Jina form (plain httpx against
``<base>v1/rerank``) must echo the documents by default and score ``{text}``
objects exactly like strings. An unknown request key is a 400
``unsupported_parameter`` on both paths, and the SDK raises it as its
BadRequestError. Without the routes the SDK meets a plain-text 404 (no JSON,
``ApiError``) and the Jina form the /v1 catch-all's 404 (gate G1 red).
"""

import json
import math
import os

import cohere
import httpx

BASE = os.environ["CTX_RERANK_BASE_URL"]
QUERY = "What is the capital of the United States?"
# The fixture of conformance_rerank_test.go (cfRerankDocs): each document and
# the logit the fake engine answers for it.
DOCS = [
    ("Carson City is the capital city of the American state of Nevada.", -1.0),
    ("Saipan is the capital of the Northern Mariana Islands.", -2.5),
    ("Capitalization is the use of a capital letter at the start of a word.", -6.0),
    ("Washington, D.C. is the capital of the United States.", 7.5),
    ("Capital gains are profits from the sale of an asset.", -1.0),
]
TEXTS = [text for text, _ in DOCS]
# descending by score, the tie of documents 0 and 4 in index order
ORDER = [3, 0, 4, 1, 2]


def _sigmoid(x):
    return 1 / (1 + math.exp(-x))


def _cohere():
    return cohere.ClientV2(api_key="ctxm_conformance", base_url=BASE, timeout=30, max_retries=0)


def _jina(body):
    return httpx.post(f"{BASE}v1/rerank", json=body, headers={"Authorization": "Bearer ctxm_conformance"}, timeout=30)


def _check_ranking(results, order, problems, what):
    """results: (index, relevance_score) per result, in answer order."""
    prev = None
    got = []
    for k, (index, score) in enumerate(results):
        if type(index) is not int or type(score) is not float:
            problems.append(f"{what}: result {k} has index {index!r} and relevance_score {score!r}, want an int and a float")
            continue
        got.append(index)
        if not 0 <= score <= 1:
            problems.append(f"{what}: result {k} relevance_score {score} outside [0, 1]")
        if 0 <= index < len(DOCS) and abs(score - _sigmoid(DOCS[index][1])) > 1e-12:
            problems.append(f"{what}: document {index} relevance_score {score}, want the sigmoid {_sigmoid(DOCS[index][1])} of {DOCS[index][1]}")
        if prev is not None and score > prev:
            problems.append(f"{what}: result {k} score {score} above its predecessor's {prev}")
        prev = score
    if got != order:
        problems.append(f"{what}: order {got}, want {order}")


def _check_cohere(resp, order, problems, what):
    if not isinstance(resp, cohere.V2RerankResponse):
        problems.append(f"{what}: the SDK returned {type(resp).__name__}, want V2RerankResponse")
        return
    if not isinstance(resp.id, str) or not resp.id.startswith("rerank-"):
        problems.append(f"{what}: id {resp.id!r}, want rerank-<request id>")
    _check_ranking([(r.index, r.relevance_score) for r in resp.results], order, problems, what)
    if any(getattr(r, "document", None) is not None for r in resp.results):
        problems.append(f"{what}: /v2 echoed documents without return_documents")


def test_cohere_sdk_parses_the_v2_answer():
    problems = []
    client = _cohere()
    _check_cohere(client.rerank(model="rr", query=QUERY, documents=TEXTS), ORDER, problems, "all")
    raw = client.with_raw_response.rerank(model="rr", query=QUERY, documents=TEXTS, top_n=3)
    if raw.headers.get("content-type") != "application/json":
        problems.append(f"Content-Type {raw.headers.get('content-type')!r}, want application/json")
    sent = sorted(json.loads(raw.response.request.content))
    if sent != ["documents", "model", "query", "top_n"]:
        problems.append(f"the SDK sent the members {sent}, want documents, model, query, top_n")
    if any("document" in r for r in raw.response.json()["results"]):
        problems.append("top_n=3: a document member on the wire of /v2 without return_documents")
    _check_cohere(raw.data, ORDER[:3], problems, "top_n=3")
    assert not problems, "; ".join(problems)


def test_jina_form_echoes_documents_and_scores_objects_like_strings():
    problems = []
    answers = {}
    for form, documents, top_n in (
        ("strings", TEXTS, None),
        ("objects", [{"text": t} for t in TEXTS], None),
        ("top_n=2", TEXTS, 2),
    ):
        body = {"model": "rr", "query": QUERY, "documents": documents}
        if top_n is not None:
            body["top_n"] = top_n
        r = _jina(body)
        if r.status_code != 200 or r.headers.get("content-type") != "application/json":
            problems.append(f"{form}: {r.status_code} {r.headers.get('content-type')} {r.text[:200]!r}, want 200 application/json")
            continue
        answer = r.json()
        answers[form] = answer["results"]
        if answer.get("model") != "rr" or not str(answer.get("id", "")).startswith("rerank-"):
            problems.append(f"{form}: model {answer.get('model')!r} id {answer.get('id')!r}")
        usage = answer.get("usage", {})
        if not usage.get("prompt_tokens", 0) > 0 or usage.get("total_tokens") != usage.get("prompt_tokens"):
            problems.append(f"{form}: usage {usage!r}")
        _check_ranking([(x.get("index"), x.get("relevance_score")) for x in answer["results"]],
                       ORDER[:top_n] if top_n else ORDER, problems, form)
        for x in answer["results"]:
            doc = x.get("document")
            if not isinstance(doc, dict) or doc.get("text") != TEXTS[x["index"]]:
                problems.append(f"{form}: document {x.get('index')} echo {doc!r}, want {{'text': {TEXTS[x['index']]!r}}}")
    if "strings" in answers and answers.get("objects") != answers["strings"]:
        problems.append("{text} objects were answered differently from strings")
    assert not problems, "; ".join(problems)


def test_unknown_key_is_unsupported_parameter():
    problems = []
    r = _jina({"model": "rr", "query": QUERY, "documents": TEXTS, "rank_fields": ["text"]})
    err = r.json().get("error", {}) if r.headers.get("content-type") == "application/json" else {}
    if r.status_code != 400 or err.get("code") != "unsupported_parameter" or err.get("param") != "rank_fields":
        problems.append(f"Jina: {r.status_code} {r.text[:200]!r}, want 400 unsupported_parameter for rank_fields")
    try:
        _cohere().rerank(model="rr", query=QUERY, documents=TEXTS,
                         request_options={"additional_body_parameters": {"rank_fields": ["text"]}})
    except cohere.BadRequestError as e:
        err = e.body.get("error", {}) if isinstance(e.body, dict) else {}
        if err.get("code") != "unsupported_parameter" or err.get("param") != "rank_fields":
            problems.append(f"Cohere SDK: BadRequestError body {e.body!r}, want error.code unsupported_parameter for rank_fields")
    else:
        problems.append("Cohere SDK: an unknown key was accepted")
    assert not problems, "; ".join(problems)
