"""Live smoke of the OpenAI-compatible API against a RUNNING ctx (waves W02-15,
W06-19 stage 2, W06-20; design/02 §7 W02-15, design/06 §7 W06-19/W06-20).

The in-process harness (internal/oai/pyconformance_test.go) checks the
handlers against fakes; this script checks a deployed stack the way an
external program meets it. test.sh T25 runs ``t25``; the release checklist
(checklist.json) names the other subcommands. Standard library plus the
official OpenAI Python SDK for ``run``/``t25`` (``uv run --no-project --with
openai==3.42.1``); keys come from environment variables only, never from the
command line.

Subcommands:

``--list``
    print the subcommand ids one per line (the checklist's ``tool`` column).
``preflight --base-url URL --admin-key-env NAME [--partition spark-chat=4,spark-embed=1]``
    /health reachable (version printed); no ``foreign_load`` row of the
    server-admin's /api/status suspected (live or hourly); every row named in
    ``--partition`` carries that ``limits.internal_reserved_slots`` and an
    ``abort_settle_ms`` (backend-list).
``flip-probe --base-url URL --expect off|on``
    anonymous ``POST /v1/chat/completions``: ``off`` wants 404 not_found in
    the OpenAI envelope with x-should-retry false (a 401 means the door runs
    before the gate); ``on`` wants 401 invalid_api_key with
    ``WWW-Authenticate: Bearer realm="ctx"``; a 503 proxy_misconfigured names
    CTX_TRUSTED_PROXY.
``run --base-url URL --key-env NAME [--chat-model M --embed-model M --rerank-model M]``
    the probes with a model key: models, chat (with the meter's six
    x-ratelimit-* headers), chat_stream, embeddings, rerank, unauth,
    envelope. A model not given is taken from /v1/models by
    its ``ctx.kind``; a kind the key sees no model of is a FAIL.
``t25 --base-url URL --admin-key-env NAME``
    anonymous probe first — 404 not_found ⇒ SKIP (gateway.enabled is false;
    no consumer is created); otherwise create a test consumer
    (model-consumer-create, all_models), ``run`` with its key, delete it
    (model-consumer-delete) in every case.

Exit codes: 0 PASS · 77 SKIP (reason on the last line, ``SKIP: …``) ·
1 FAIL (``FAIL: <probe>: <reason>``) · 2 usage. The last stdout line is the
verdict; ``--json`` adds one JSON object per probe before it.
"""

import argparse
import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.request

EXIT_PASS = 0
EXIT_FAIL = 1
EXIT_USAGE = 2
EXIT_SKIP = 77

SUBCOMMANDS: list[str] = ["preflight", "flip-probe", "run", "t25"]

TIMEOUT_S = 60.0
RATELIMIT_HEADERS = ("x-ratelimit-limit-requests", "x-ratelimit-remaining-requests", "x-ratelimit-reset-requests",
                     "x-ratelimit-limit-tokens", "x-ratelimit-remaining-tokens", "x-ratelimit-reset-tokens")
CHAT_BODY = {"model": "smoke", "messages": [{"role": "user", "content": "Say hi."}], "max_tokens": 8}


class ProbeFailure(Exception):
    """A probe's failure: str(e) is the reason, e.probe the probe id."""

    def __init__(self, probe: str, reason: str):
        super().__init__(reason)
        self.probe = probe


def _ssl_context() -> ssl.SSLContext | None:
    # CTX_SMOKE_INSECURE=1 accepts the self-signed certificate of a local
    # reverse proxy (https://localhost); off by default.
    if os.environ.get("CTX_SMOKE_INSECURE") == "1":
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        return ctx
    return None


def http(method: str, url: str, body=None, headers=None, timeout: float = TIMEOUT_S):
    """One request without retries: (status, headers, raw body)."""
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, method=method, headers=dict(headers or {}))
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout, context=_ssl_context()) as r:
            return r.status, r.headers, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read()


def envelope(raw: bytes) -> dict | None:
    """The OpenAI error envelope's inner object, or None if raw is none."""
    try:
        doc = json.loads(raw)
    except ValueError:
        return None
    err = doc.get("error") if isinstance(doc, dict) else None
    if not isinstance(err, dict) or set(doc) != {"error"}:
        return None
    if not isinstance(err.get("message"), str) or not isinstance(err.get("type"), str) or "code" not in err or "param" not in err:
        return None
    return err


def base(url: str) -> str:
    return url.rstrip("/")


# ── flip-probe ──────────────────────────────────────────────────────────────

def flip_probe(base_url: str, expect: str) -> str:
    status, hdr, raw = http("POST", base(base_url) + "/v1/chat/completions", CHAT_BODY)
    env = envelope(raw)
    code = env.get("code") if env else None
    if status == 503 and code == "proxy_misconfigured":
        raise ProbeFailure("flip-probe", "503 proxy_misconfigured — X-Forwarded-For arrives and CTX_TRUSTED_PROXY is unset; "
                                         "set CTX_TRUSTED_PROXY to the proxy's address as ctxd sees it")
    if env is None:
        raise ProbeFailure("flip-probe", f"{status} without the OpenAI error envelope: {raw[:120]!r}")
    if hdr.get("x-should-retry") is None:
        raise ProbeFailure("flip-probe", f"{status} {code} without x-should-retry")
    if expect == "off":
        if status == 404 and code == "not_found":
            if hdr.get("x-should-retry") != "false":
                raise ProbeFailure("flip-probe", "404 not_found with x-should-retry other than false")
            return "PASS: flag off — anonymous POST /v1/chat/completions is 404 not_found before the door"
        if status == 401:
            raise ProbeFailure("flip-probe", "401 with the flag off — the door runs before the gate (or gateway.enabled is true)")
        raise ProbeFailure("flip-probe", f"{status} {code}, want 404 not_found with the flag off")
    if status == 401 and code == "invalid_api_key":
        if hdr.get("WWW-Authenticate") != 'Bearer realm="ctx"':
            raise ProbeFailure("flip-probe", f"401 without WWW-Authenticate: Bearer realm=\"ctx\" ({hdr.get('WWW-Authenticate')!r})")
        return "PASS: flag on — anonymous POST /v1/chat/completions is 401 invalid_api_key"
    if status == 404 and code == "not_found":
        raise ProbeFailure("flip-probe", "404 not_found — gateway.enabled is still false")
    raise ProbeFailure("flip-probe", f"{status} {code}, want 401 invalid_api_key with the flag on")


# ── run ─────────────────────────────────────────────────────────────────────

def _need(cond: bool, probe: str, reason: str):
    if not cond:
        raise ProbeFailure(probe, reason)


def _json(probe: str, status: int, raw: bytes) -> dict:
    _need(status == 200, probe, f"status {status}: {raw[:160]!r}")
    try:
        doc = json.loads(raw)
    except ValueError:
        raise ProbeFailure(probe, f"answer is not JSON: {raw[:120]!r}") from None
    _need(isinstance(doc, dict), probe, "answer is not a JSON object")
    return doc


def _usage(probe: str, u) -> None:
    _need(isinstance(u, dict) and isinstance(u.get("total_tokens"), int) and u["total_tokens"] > 0,
          probe, f"usage {u!r}, want total_tokens > 0")


def probe_models(b: str, auth: dict) -> dict[str, str]:
    """GET /v1/models: 200 list; returns the first model id per ctx.kind."""
    status, _, raw = http("GET", b + "/v1/models", headers=auth)
    doc = _json("models", status, raw)
    _need(doc.get("object") == "list" and isinstance(doc.get("data"), list), "models", f"not a list answer: {raw[:160]!r}")
    kinds: dict[str, str] = {}
    for m in doc["data"]:
        _need(isinstance(m, dict) and isinstance(m.get("id"), str) and m.get("object") == "model"
              and isinstance(m.get("created"), int) and isinstance(m.get("owned_by"), str), "models", f"malformed model entry {m!r}")
        kind = (m.get("ctx") or {}).get("kind") if isinstance(m.get("ctx"), dict) else None
        if isinstance(kind, str):
            kinds.setdefault(kind, m["id"])
    return kinds


def probe_chat(b: str, auth: dict, model: str, sdk) -> None:
    body = dict(CHAT_BODY, model=model)
    status, hdr, raw = http("POST", b + "/v1/chat/completions", body, auth)
    doc = _json("chat", status, raw)
    # The consumer's meter (W06-10) answers on every served chat.
    missing = [h for h in RATELIMIT_HEADERS if hdr.get(h) is None]
    _need(not missing, "chat", f"served chat without {missing}")
    ch = doc.get("choices")
    _need(doc.get("object") == "chat.completion" and isinstance(ch, list) and ch and isinstance(ch[0], dict),
          "chat", f"not a chat.completion: {raw[:160]!r}")
    msg = ch[0].get("message")
    _need(isinstance(msg, dict) and isinstance(msg.get("content"), str), "chat", f"message without content: {msg!r}")
    _need("reasoning_content" not in msg, "chat", "message carries reasoning_content (thinking must be off)")
    _usage("chat", doc.get("usage"))
    if sdk is not None:
        r = sdk.chat.completions.create(model=model, messages=body["messages"], max_tokens=8)
        _need(isinstance(r.choices[0].message.content, str), "chat", "the SDK parsed no message content")


def probe_chat_stream(b: str, auth: dict, model: str, sdk) -> None:
    if sdk is None:
        raise ProbeFailure("chat_stream", "the OpenAI SDK is not installed")
    content, usage, chunks = "", None, 0
    stream = sdk.chat.completions.create(model=model, messages=CHAT_BODY["messages"], max_tokens=8, stream=True,
                                         stream_options={"include_usage": True})
    for c in stream:
        chunks += 1
        _need(c.object == "chat.completion.chunk", "chat_stream", f"chunk object {c.object!r}")
        for choice in c.choices:
            d = choice.delta
            _need(getattr(d, "reasoning_content", None) is None and "reasoning_content" not in (d.model_extra or {}),
                  "chat_stream", "a delta carries reasoning_content (thinking must be off)")
            content += d.content or ""
        if c.usage is not None:
            usage = c.usage
    _need(chunks > 0 and content != "", "chat_stream", f"{chunks} chunk(s), no content")
    _need(usage is not None and usage.total_tokens > 0, "chat_stream", f"no usage chunk with total_tokens > 0 ({usage!r})")


def probe_embeddings(b: str, auth: dict, model: str) -> None:
    body = {"model": model, "input": ["ctx smoke one", "ctx smoke two"], "encoding_format": "float"}
    status, _, raw = http("POST", b + "/v1/embeddings", body, auth)
    doc = _json("embeddings", status, raw)
    data = doc.get("data")
    _need(doc.get("object") == "list" and isinstance(data, list) and len(data) == 2, "embeddings", f"want 2 embeddings: {raw[:160]!r}")
    for i, e in enumerate(data):
        v = e.get("embedding") if isinstance(e, dict) else None
        _need(e.get("index") == i and isinstance(v, list) and v and all(isinstance(x, (int, float)) for x in v),
              "embeddings", f"entry {i} malformed")
    _need(len(data[0]["embedding"]) == len(data[1]["embedding"]), "embeddings", "two dimensions in one answer")
    _usage("embeddings", doc.get("usage"))


def probe_rerank(b: str, auth: dict, model: str) -> None:
    docs = ["Berlin is the capital of Germany.", "Bananas are yellow."]
    status, _, raw = http("POST", b + "/v1/rerank", {"model": model, "query": "capital of Germany", "documents": docs}, auth)
    doc = _json("rerank", status, raw)
    res = doc.get("results")
    _need(isinstance(res, list) and len(res) == 2, "rerank", f"want 2 results: {raw[:160]!r}")
    for r in res:
        _need(isinstance(r, dict) and r.get("index") in (0, 1) and isinstance(r.get("relevance_score"), (int, float))
              and 0 <= r["relevance_score"] <= 1, "rerank", f"malformed result {r!r}")
    _need(res[0]["relevance_score"] >= res[1]["relevance_score"], "rerank", "results not in descending order")
    _usage("rerank", doc.get("usage"))


def probe_unauth(b: str) -> None:
    status, hdr, raw = http("POST", b + "/v1/chat/completions", CHAT_BODY)
    env = envelope(raw)
    _need(env is not None, "unauth", f"{status} without the OpenAI error envelope: {raw[:120]!r}")
    _need(status == 401 and env.get("code") == "invalid_api_key", "unauth", f"{status} {env.get('code')}, want 401 invalid_api_key")
    _need(hdr.get("WWW-Authenticate") == 'Bearer realm="ctx"', "unauth", "401 without WWW-Authenticate")
    _need(hdr.get("x-should-retry") == "false", "unauth", "401 without x-should-retry: false")


def probe_envelope(b: str, auth: dict) -> None:
    status, hdr, raw = http("GET", b + "/v1/does-not-exist", headers=auth)
    env = envelope(raw)
    _need(env is not None and status == 404 and env.get("code") == "not_found", "envelope",
          f"unknown path: {status} {raw[:120]!r}, want 404 not_found in the OpenAI envelope")
    _need(hdr.get("x-should-retry") == "false", "envelope", "404 without x-should-retry: false")


def _sdk(b: str, key: str):
    try:
        import openai  # noqa: PLC0415 — optional: only run/t25 need it
    except ImportError:
        return None
    # The SDK verifies TLS itself; CTX_SMOKE_INSECURE covers the raw probes
    # only (the shipped reverse proxy serves a certificate curl accepts).
    return openai.OpenAI(base_url=b + "/v1/", api_key=key, max_retries=0, timeout=TIMEOUT_S)


def run(base_url: str, key: str, chat: str | None, embed: str | None, rerank: str | None, report) -> str:
    b = base(base_url)
    auth = {"Authorization": f"Bearer {key}"}
    sdk = _sdk(b, key)
    if sdk is None:
        raise ProbeFailure("run", "the OpenAI Python SDK is not installed (uv run --no-project --with openai==3.42.1 …)")
    kinds = probe_models(b, auth)
    report("models", True)
    chat = chat or kinds.get("chat")
    embed = embed or kinds.get("embedding")
    rerank = rerank or kinds.get("rerank")
    for probe, model in (("chat", chat), ("embeddings", embed), ("rerank", rerank)):
        _need(model is not None, probe, f"the key sees no model of this kind ({sorted(kinds)})")
    probe_chat(b, auth, chat, sdk)
    report("chat", True)
    probe_chat_stream(b, auth, chat, sdk)
    report("chat_stream", True)
    probe_embeddings(b, auth, embed)
    report("embeddings", True)
    probe_rerank(b, auth, rerank)
    report("rerank", True)
    probe_unauth(b)
    report("unauth", True)
    probe_envelope(b, auth)
    report("envelope", True)
    return f"PASS: 7 probes (chat {chat}, embeddings {embed}, rerank {rerank})"


# ── t25 ─────────────────────────────────────────────────────────────────────

def manage(b: str, admin: str, action: str, data: dict) -> dict:
    status, _, raw = http("POST", b + "/api/manage", {"action": action, "data": data}, {"X-Context-Key": admin})
    try:
        doc = json.loads(raw)
    except ValueError:
        doc = {}
    if status != 200 or not doc.get("success"):
        raise ProbeFailure("t25", f"{action}: {status} {raw[:160]!r}")
    return doc


def t25(base_url: str, admin: str, chat, embed, rerank, report) -> tuple[int, str]:
    b = base(base_url)
    status, _, raw = http("GET", b + "/v1/models")
    env = envelope(raw)
    code = env.get("code") if env else None
    if status == 404 and code == "not_found":
        return EXIT_SKIP, "SKIP: gateway.enabled is false — /v1 answers 404 not_found (no test consumer created)"
    if status == 503 and code == "proxy_misconfigured":
        raise ProbeFailure("t25", "503 proxy_misconfigured — set CTX_TRUSTED_PROXY to the proxy's address as ctxd sees it")
    if not (status == 401 and code == "invalid_api_key"):
        raise ProbeFailure("t25", f"anonymous GET /v1/models: {status} {raw[:120]!r}, want 404 (flag off) or 401 (flag on)")
    created = manage(b, admin, "model-consumer-create",
                     {"label": f"__t25_probe_{int(time.time())}", "all_models": True,
                      "description": "test.sh T25 — deleted at the end of the run"})
    cid = (created.get("consumer") or {}).get("id")
    key = created.get("api_key")
    if not cid or not key:
        raise ProbeFailure("t25", "model-consumer-create answered no consumer id or key")
    failure: ProbeFailure | None = None
    verdict = ""
    try:
        verdict = run(b, key, chat, embed, rerank, report)
    except ProbeFailure as e:
        failure = e
    except Exception as e:  # noqa: BLE001 — an SDK error fails the probe; the consumer is deleted below either way
        failure = ProbeFailure("run", f"{type(e).__name__}: {e}")
    try:
        manage(b, admin, "model-consumer-delete", {"id": cid})
        report("cleanup", True)
    except ProbeFailure as e:
        # The probe's own failure stays the verdict; a failed delete is named with it.
        if failure is None:
            raise ProbeFailure("cleanup", f"test consumer {cid} not deleted: {e}") from None
        raise ProbeFailure(failure.probe, f"{failure} (and test consumer {cid} not deleted: {e})") from None
    if failure is not None:
        raise failure
    return EXIT_PASS, verdict


# ── preflight ───────────────────────────────────────────────────────────────

def preflight(base_url: str, admin: str, partition: dict[str, int], report) -> str:
    b = base(base_url)
    status, _, raw = http("GET", b + "/health")
    _need(status == 200, "preflight", f"/health {status}")
    try:
        version = json.loads(raw).get("version")
    except ValueError:
        version = None
    report("health", True, version=version)
    status, _, raw = http("GET", b + "/api/status", headers={"X-Context-Key": admin})
    doc = _json("preflight", status, raw)
    rows = doc.get("foreign_load")
    _need(rows is not None, "preflight", "/api/status has no foreign_load section (server-admin key? detector off?)")
    bad = [r.get("target") for r in rows if isinstance(r, dict) and (r.get("suspected") or r.get("suspected_hourly"))]
    _need(not bad, "preflight", f"foreign load suspected on {bad}")
    report("foreign-load", True, targets=len(rows))
    if partition:
        status, _, raw = http("POST", b + "/api/manage", {"action": "backend-list"}, {"X-Context-Key": admin})
        doc = _json("preflight", status, raw)
        by_name = {r.get("name"): r for r in doc.get("backends") or [] if isinstance(r, dict)}
        for name, want in partition.items():
            row = by_name.get(name)
            _need(row is not None, "preflight", f"backend row {name} not found")
            limits = row.get("limits") or {}
            _need(limits.get("internal_reserved_slots") == want, "preflight",
                  f"{name}: internal_reserved_slots {limits.get('internal_reserved_slots')!r}, want {want}")
            _need(isinstance(limits.get("abort_settle_ms"), dict), "preflight", f"{name}: no abort_settle_ms (A03-W11 measurement)")
        report("partition", True, rows=sorted(partition))
    return f"PASS: preflight (version {version})"


def _partition(spec: str) -> dict[str, int]:
    out: dict[str, int] = {}
    for part in filter(None, (p.strip() for p in spec.split(","))):
        name, _, n = part.partition("=")
        out[name] = int(n)
    return out


# ── CLI ─────────────────────────────────────────────────────────────────────

def _parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="smoke.py", description=__doc__.splitlines()[0])
    p.add_argument("--list", action="store_true", help="print the implemented subcommand ids")
    p.add_argument("--json", action="store_true", help="one JSON object per probe before the verdict")
    sub = p.add_subparsers(dest="cmd")
    pre = sub.add_parser("preflight")
    pre.add_argument("--base-url", required=True)
    pre.add_argument("--admin-key-env", required=True)
    pre.add_argument("--partition", default="", help="name=slots,… (e.g. spark-chat=4,spark-embed=1)")
    fp = sub.add_parser("flip-probe")
    fp.add_argument("--base-url", required=True)
    fp.add_argument("--expect", choices=("off", "on"), required=True)
    run_p = sub.add_parser("run")
    run_p.add_argument("--base-url", required=True)
    run_p.add_argument("--key-env", required=True)
    for sp in (run_p,):
        sp.add_argument("--chat-model")
        sp.add_argument("--embed-model")
        sp.add_argument("--rerank-model")
    t = sub.add_parser("t25")
    t.add_argument("--base-url", required=True)
    t.add_argument("--admin-key-env", required=True)
    t.add_argument("--chat-model")
    t.add_argument("--embed-model")
    t.add_argument("--rerank-model")
    return p


def _env(name: str) -> str:
    v = os.environ.get(name, "")
    if not v:
        raise ProbeFailure("config", f"environment variable {name} is empty")
    return v


def main(argv: list[str]) -> int:
    parser = _parser()
    args = parser.parse_args(argv)
    if args.list:
        for s in SUBCOMMANDS:
            print(s)
        return EXIT_PASS
    if args.cmd is None:
        parser.print_usage(sys.stderr)
        return EXIT_USAGE

    def report(probe: str, ok: bool, **kv):
        if args.json:
            print(json.dumps({"probe": probe, "ok": ok, **kv}))

    try:
        if args.cmd == "flip-probe":
            print(flip_probe(args.base_url, args.expect))
            return EXIT_PASS
        if args.cmd == "run":
            print(run(args.base_url, _env(args.key_env), args.chat_model, args.embed_model, args.rerank_model, report))
            return EXIT_PASS
        if args.cmd == "t25":
            rc, verdict = t25(args.base_url, _env(args.admin_key_env), args.chat_model, args.embed_model, args.rerank_model, report)
            print(verdict)
            return rc
        if args.cmd == "preflight":
            print(preflight(args.base_url, _env(args.admin_key_env), _partition(args.partition), report))
            return EXIT_PASS
    except ProbeFailure as e:
        print(f"FAIL: {e.probe}: {e}")
        return EXIT_FAIL
    except (OSError, ValueError) as e:
        print(f"FAIL: {args.cmd}: {type(e).__name__}: {e}")
        return EXIT_FAIL
    except Exception as e:  # noqa: BLE001 — an SDK error is a probe failure, never a traceback as verdict
        print(f"FAIL: {args.cmd}: {type(e).__name__}: {e}")
        return EXIT_FAIL
    parser.print_usage(sys.stderr)
    return EXIT_USAGE


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
