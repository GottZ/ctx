"""Live measurements of the release wave W02-15 against a RUNNING ctx
(design/02 §7 W02-15; masterplan §8 open measurements; checklist.json).

Run once by the main context in RC-D S4, never in CI. Engine-side
measurements (SGLang num_running_reqs after a client abort, n = 8,
array-embed slots) are the bench tool of A03-W11 outside the repository
(.project/plan-openai-api-2026-11-03/bench/); this script measures what a
client of ctx sees. Standard library only; keys from environment variables.

Each subcommand prints ONE JSON object on stdout (the last line) and exits 0
when the checklist threshold holds, 1 when it does not or the measurement
failed, 2 on usage:

``--list``
    print the subcommand ids one per line.
``proxy --base-url URL [--admin-key-env NAME --proxy-addr ADDR]``
    a request through the reverse proxy with a malformed model key and its
    own ``X-Forwarded-For``:
    ``{"verdict": "trusted_proxy_unset"}`` (503 proxy_misconfigured — exit 1)
    or ``{"verdict": "trusted_proxy_set"}`` (the door's 401). Set is not
    right: with ``--admin-key-env`` and ``--proxy-addr`` a test consumer
    whose ``allowed_cidrs`` holds only the proxy's address must be refused
    (``"allowlist": "judges_client"`` — exit 0); a 200 is
    ``"allowlist": "sees_proxy"`` (exit 1: CTX_TRUSTED_PROXY holds a wrong
    address and the door takes the proxy for the client). Without the two
    flags ``"allowlist": "unchecked"`` (exit 0 — stage 1 only);
    ``{"verdict": "gateway_off"}`` (404 not_found — exit 1: the door and its
    proxy check sit behind the flag, so the probe runs right after the flip;
    a misconfigured proxy is fail-closed there, every keyed request a 503);
    anything else ``{"verdict": "unexpected", "status": …}`` (exit 1).
``prefill --base-url URL --key-env NAME --model M --tokens N``
    one streamed chat whose prompt has about N tokens:
    ``{"tokens": N, "first_byte_s": …, "prefill_s": …, "total_s": …,
    "cut_off": bool}`` — prefill_s is the time to the first content delta;
    exit 1 when the stream ended (or errored) before its first token.
``heap --base-url URL --key-env NAME --model M --streams 64 --consumers-keys-env NAME --mem-file PATH --budget-bytes B``
    flood of ``--streams`` concurrent streamed chats, round-robin over the
    comma-separated keys in ``--consumers-keys-env`` (8 consumers live; the
    one key of ``--key-env`` when that variable is empty),
    while sampling ``--mem-file`` (a cgroup ``memory.current`` of the ctx
    container, or a file a sampler rewrites) every 50 ms from before the
    flood to its end: ``{"base_bytes", "peak_bytes", "budget_bytes",
    "streams", "failed_streams", "samples", "ok"}``, ok = peak − base <
    budget.
``retry-after --base-url URL --key-env NAME --model M --requests N [--concurrency C]``
    N non-streamed chats (C at a time):
    ``{"requests", "status": {…}, "ctx_429", "proxy_429", "proxy_servers":
    {…}, "share_429", "retry_after": [...], "max_allowed", "violations":
    [...], "ok"}`` — ok = at least one 429 of ctx itself and every one
    carries a whole-second Retry-After in [1, ceil(30 × (1 + --jitter))]
    (45 s at the default jitter 0.5) and an x-should-retry header. A 429
    of a reverse proxy in front of ctx (its own page: neither a JSON body
    nor x-should-retry — openresty's limit_req answers HTML) counts as
    ``proxy_429``, outside ``violations``, ``share_429`` (ctx's 429 over
    the requests ctx answered: ``ctx_429 / (requests − proxy_429)``) and
    ``retry_after``; ``status`` counts every answer. The Server header
    tells nothing apart: behind the proxy ctx's own answers carry the
    proxy's Server too. A run whose 429 are all the proxy's never reached
    the door's limit — exit 1 with an ``error``, not a pass.
"""

import argparse
import datetime
import json
import math
import os
import ssl
import sys
import threading
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

EXIT_PASS = 0
EXIT_FAIL = 1
EXIT_USAGE = 2

SUBCOMMANDS: list[str] = ["proxy", "prefill", "heap", "retry-after"]

TIMEOUT_S = 300.0
# The L2 Retry-After ladder tops out at 30 s; the gateway multiplies a hint by
# U[1, 1 + gateway.retry_after_jitter] (default 0.5, config.go
# RetryAfterJitter), so a valid header lies in [1, ceil(30 × (1 + jitter))].
RETRY_AFTER_MIN, RETRY_AFTER_STEP_MAX, RETRY_AFTER_JITTER = 1, 30, 0.5
# The address the proxy probe puts into X-Forwarded-For (TEST-NET-1, RFC 5737).
PROBE_XFF = "192.0.2.1"


def _ssl_context() -> ssl.SSLContext | None:
    if os.environ.get("CTX_SMOKE_INSECURE") == "1":
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        return ctx
    return None


def _request(method: str, url: str, body=None, headers=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, method=method, headers=dict(headers or {}))
    if data is not None:
        req.add_header("Content-Type", "application/json")
    return req


def http(method: str, url: str, body=None, headers=None, timeout: float = TIMEOUT_S):
    try:
        with urllib.request.urlopen(_request(method, url, body, headers), timeout=timeout, context=_ssl_context()) as r:
            return r.status, r.headers, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read()


def _code(raw: bytes):
    try:
        err = json.loads(raw).get("error")
        return err.get("code") if isinstance(err, dict) else None
    except (ValueError, AttributeError):
        return None


def _env(name: str) -> str:
    v = os.environ.get(name, "")
    if not v:
        raise ValueError(f"environment variable {name} is empty")
    return v


# ── proxy ───────────────────────────────────────────────────────────────────

def _manage(b: str, admin: str, action: str, data: dict) -> dict:
    status, _, raw = http("POST", b + "/api/manage", {"action": action, "data": data}, {"X-Context-Key": admin}, timeout=30)
    try:
        doc = json.loads(raw)
    except ValueError:
        doc = {}
    if status != 200 or not doc.get("success"):
        raise ValueError(f"{action}: {status} {raw[:160]!r}")
    return doc


def _allowlist(b: str, admin: str, proxy_addr: str) -> dict:
    """A consumer whose allowed_cidrs holds ONLY the proxy's address: the
    door must refuse it (401) — it judges the client behind the proxy. A 200
    means the door takes the proxy for the client (CTX_TRUSTED_PROXY set to
    a wrong value): every allowlist naming the proxy or its network is open."""
    bits = 128 if ":" in proxy_addr else 32
    expires = (datetime.datetime.now(datetime.UTC) + datetime.timedelta(minutes=15)).strftime("%Y-%m-%dT%H:%M:%SZ")
    created = _manage(b, admin, "model-consumer-create",
                      {"label": f"__proxy_probe_{int(time.time())}", "all_models": True, "expires_at": expires,
                       "allowed_cidrs": [f"{proxy_addr}/{bits}"], "description": "measure.py proxy — deleted at the end"})
    cid = (created.get("consumer") or {}).get("id")
    key = created.get("api_key")
    try:
        if not cid or not key:
            raise ValueError("model-consumer-create answered no consumer id or key")
        status, _, raw = http("GET", b + "/v1/models", headers={"Authorization": f"Bearer {key}"}, timeout=30)
    finally:
        if cid:
            _manage(b, admin, "model-consumer-delete", {"id": cid})
    if status == 401:
        return {"allowlist": "judges_client", "allowlist_status": status}
    if status == 200:
        return {"allowlist": "sees_proxy", "allowlist_status": status}
    return {"allowlist": "unexpected", "allowlist_status": status, "allowlist_code": _code(raw)}


def proxy(base_url: str, admin: str | None = None, proxy_addr: str | None = None) -> tuple[bool, dict]:
    b = base_url.rstrip("/")
    status, _, raw = http("POST", b + "/v1/chat/completions",
                          {"model": "probe", "messages": [{"role": "user", "content": "x"}]},
                          {"Authorization": "Bearer ctxm_proxy-probe-not-a-key", "X-Forwarded-For": PROBE_XFF}, timeout=30)
    code = _code(raw)
    if status == 503 and code == "proxy_misconfigured":
        return False, {"verdict": "trusted_proxy_unset", "status": status, "code": code}
    if status == 404 and code == "not_found":
        return False, {"verdict": "gateway_off", "status": status, "code": code,
                       "note": "the door is behind gateway.enabled — probe right after the flip"}
    if not (status == 401 and code == "invalid_api_key"):
        return False, {"verdict": "unexpected", "status": status, "code": code}
    res = {"verdict": "trusted_proxy_set", "status": status, "code": code}
    if admin is None or proxy_addr is None:
        # Stage 1 passes: the variable is set — whether to the RIGHT address
        # only the allowlist step tells (--admin-key-env and --proxy-addr).
        res["allowlist"] = "unchecked"
        return True, res
    res.update(_allowlist(b, admin, proxy_addr))
    return res["allowlist"] == "judges_client", res


# ── streaming ───────────────────────────────────────────────────────────────

def stream_chat(base_url: str, key: str, model: str, prompt: str, max_tokens: int = 8) -> dict:
    """One streamed chat: first-byte and first-token times, end state."""
    body = {"model": model, "messages": [{"role": "user", "content": prompt}], "max_tokens": max_tokens,
            "stream": True, "stream_options": {"include_usage": True}}
    req = _request("POST", base_url.rstrip("/") + "/v1/chat/completions", body, {"Authorization": f"Bearer {key}"})
    t0 = time.monotonic()
    out = {"status": None, "first_byte_s": None, "prefill_s": None, "total_s": None, "done": False, "error": None}
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT_S, context=_ssl_context()) as r:
            out["status"] = r.status
            out["first_byte_s"] = time.monotonic() - t0
            for line in r:
                line = line.strip()
                if not line.startswith(b"data:"):
                    continue
                payload = line[5:].strip()
                if payload == b"[DONE]":
                    out["done"] = True
                    break
                try:
                    frame = json.loads(payload)
                except ValueError:
                    out["error"] = "malformed frame"
                    break
                if "error" in frame:
                    out["error"] = (frame.get("error") or {}).get("code") or "error frame"
                    break
                for ch in frame.get("choices") or []:
                    if (ch.get("delta") or {}).get("content") and out["prefill_s"] is None:
                        out["prefill_s"] = time.monotonic() - t0
    except urllib.error.HTTPError as e:
        out["status"] = e.code
        out["error"] = _code(e.read()) or f"http {e.code}"
    except OSError as e:
        out["error"] = f"{type(e).__name__}: {e}"
    out["total_s"] = time.monotonic() - t0
    return out


def prefill(base_url: str, key: str, model: str, tokens: int) -> tuple[bool, dict]:
    # About one token per short word for the common BPE vocabularies; the
    # engine's prompt_tokens is not read here — the point is the wall time.
    prompt = "ctx " * max(1, tokens - 16) + "\nAnswer with one word."
    r = stream_chat(base_url, key, model, prompt)
    # cut_off: the stream was admitted (200) and ended before its first token;
    # a refusal (4xx/5xx) is an error, not a cut.
    cut = r["status"] == 200 and r["prefill_s"] is None
    res = {"tokens": tokens, "first_byte_s": r["first_byte_s"], "prefill_s": r["prefill_s"], "total_s": r["total_s"],
           "cut_off": cut, "status": r["status"], "error": r["error"]}
    return r["status"] == 200 and not cut, res


# ── heap ────────────────────────────────────────────────────────────────────

def _read_bytes(path: str) -> int | None:
    try:
        with open(path) as f:
            return int(f.read().split()[0])
    except (OSError, ValueError, IndexError):
        return None


def heap(base_url: str, keys: list[str], model: str, streams: int, mem_file: str, budget: int) -> tuple[bool, dict]:
    base_bytes = _read_bytes(mem_file)
    if base_bytes is None:
        return False, {"error": f"cannot read {mem_file}"}
    samples = [base_bytes]
    stop = threading.Event()

    def sampler():
        while not stop.is_set():
            v = _read_bytes(mem_file)
            if v is not None:
                samples.append(v)
            stop.wait(0.05)

    th = threading.Thread(target=sampler, daemon=True)
    th.start()
    with ThreadPoolExecutor(max_workers=streams) as pool:
        results = list(pool.map(lambda i: stream_chat(base_url, keys[i % len(keys)], model, f"Count to {i % 7 + 3}.", 32),
                                range(streams)))
    stop.set()
    th.join()
    peak = max(samples)
    failed = sum(1 for r in results if not r["done"])
    ok = peak - base_bytes < budget
    return ok, {"base_bytes": base_bytes, "peak_bytes": peak, "budget_bytes": budget, "streams": streams,
                "failed_streams": failed, "samples": len(samples), "ok": ok}


# ── retry-after ─────────────────────────────────────────────────────────────

def _from_ctx(should_retry, raw: bytes) -> bool:
    """Whether a 429 is ctx's own answer, not a reverse proxy's in front of it.

    ctx answers every /v1 refusal with a JSON body (the OpenAI error envelope)
    and x-should-retry; a proxy's limit (openresty limit_req, S4 2026-11-08:
    166 of 200 under a burst) answers its own HTML page with neither. Either
    sign of ctx counts as ctx — a JSON 429 without the envelope or a non-JSON
    429 with x-should-retry is ctx's and is judged, never hidden as the
    proxy's. The Server header is no sign: behind the proxy it names the
    proxy on ctx's answers too (live 2026-11-08: the door's 401 and
    /api/status both say openresty)."""
    if should_retry is not None:
        return True
    try:
        json.loads(raw)
    except ValueError:
        return False
    return True


def retry_after(base_url: str, key: str, model: str, requests: int, concurrency: int,
                jitter: float = RETRY_AFTER_JITTER) -> tuple[bool, dict]:
    max_allowed = math.ceil(RETRY_AFTER_STEP_MAX * (1 + jitter))
    body = {"model": model, "messages": [{"role": "user", "content": "hi"}], "max_tokens": 4}
    url = base_url.rstrip("/") + "/v1/chat/completions"

    def one(_):
        st, hdr, raw = http("POST", url, body, {"Authorization": f"Bearer {key}"})
        return st, hdr.get("Retry-After"), hdr.get("x-should-retry"), hdr.get("Server"), raw

    with ThreadPoolExecutor(max_workers=max(1, concurrency)) as pool:
        got = list(pool.map(one, range(requests)))
    status: dict[str, int] = {}
    servers: dict[str, int] = {}
    values, violations = [], []
    ctx_429 = 0
    for st, ra, sr, server, raw in got:
        status[str(st)] = status.get(str(st), 0) + 1
        if st != 429:
            continue
        if not _from_ctx(sr, raw):
            servers[server or ""] = servers.get(server or "", 0) + 1
            continue
        ctx_429 += 1
        if sr not in ("true", "false"):
            violations.append(f"429 without x-should-retry ({sr!r})")
        try:
            n = int(ra)
        except (TypeError, ValueError):
            violations.append(f"429 with Retry-After {ra!r}")
            continue
        values.append(n)
        if not RETRY_AFTER_MIN <= n <= max_allowed:
            violations.append(f"429 with Retry-After {n} outside [{RETRY_AFTER_MIN}, {max_allowed}]")
    proxy_429 = sum(servers.values())
    # The proxy's 429 never reached ctx: the share is over ctx's answers only.
    answered = requests - proxy_429
    ok = ctx_429 > 0 and not violations
    res = {"requests": requests, "status": status, "ctx_429": ctx_429, "proxy_429": proxy_429, "proxy_servers": servers,
           "share_429": ctx_429 / answered if answered else 0.0, "retry_after": sorted(set(values)),
           "max_allowed": max_allowed, "violations": violations[:10], "ok": ok}
    if ctx_429 == 0:
        # Nothing of ctx's to judge: the flood never reached the door's limit
        # (or the proxy's limit stood in front of it) — not established, no pass.
        res["error"] = (f"no ctx 429 under the flood ({proxy_429} from a proxy in front of ctx) — "
                        "the door's limit was not reached; raise --requests/--concurrency or lower the consumer's limits")
    return ok, res


# ── CLI ─────────────────────────────────────────────────────────────────────

def _parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="measure.py", description=__doc__.splitlines()[0])
    p.add_argument("--list", action="store_true", help="print the implemented subcommand ids")
    sub = p.add_subparsers(dest="cmd")
    px = sub.add_parser("proxy")
    px.add_argument("--base-url", required=True)
    px.add_argument("--admin-key-env", help="with --proxy-addr: the allowlist step (a consumer limited to the proxy's address)")
    px.add_argument("--proxy-addr", help="the reverse proxy's address as ctxd sees it (the value CTX_TRUSTED_PROXY should hold)")
    pf = sub.add_parser("prefill")
    pf.add_argument("--base-url", required=True)
    pf.add_argument("--key-env", required=True)
    pf.add_argument("--model", required=True)
    pf.add_argument("--tokens", type=int, default=200_000)
    hp = sub.add_parser("heap")
    hp.add_argument("--base-url", required=True)
    hp.add_argument("--key-env", required=True)
    hp.add_argument("--model", required=True)
    hp.add_argument("--streams", type=int, default=64)
    hp.add_argument("--consumers-keys-env", required=True)
    hp.add_argument("--mem-file", required=True)
    hp.add_argument("--budget-bytes", type=int, required=True)
    ra = sub.add_parser("retry-after")
    ra.add_argument("--base-url", required=True)
    ra.add_argument("--key-env", required=True)
    ra.add_argument("--model", required=True)
    ra.add_argument("--requests", type=int, default=200)
    ra.add_argument("--concurrency", type=int, default=16)
    ra.add_argument("--jitter", type=float, default=RETRY_AFTER_JITTER, help="gateway.retry_after_jitter of the stack")
    return p


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
    try:
        if args.cmd == "proxy":
            admin = _env(args.admin_key_env) if args.admin_key_env else None
            ok, res = proxy(args.base_url, admin, args.proxy_addr)
        elif args.cmd == "prefill":
            ok, res = prefill(args.base_url, _env(args.key_env), args.model, args.tokens)
        elif args.cmd == "heap":
            keys = [k.strip() for k in os.environ.get(args.consumers_keys_env, "").split(",") if k.strip()] or [_env(args.key_env)]
            ok, res = heap(args.base_url, keys, args.model, args.streams, args.mem_file, args.budget_bytes)
        else:
            ok, res = retry_after(args.base_url, _env(args.key_env), args.model, args.requests, args.concurrency, args.jitter)
    except (OSError, ValueError) as e:
        ok, res = False, {"error": f"{type(e).__name__}: {e}"}
    print(json.dumps({"measure": args.cmd, **res}))
    return EXIT_PASS if ok else EXIT_FAIL


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
