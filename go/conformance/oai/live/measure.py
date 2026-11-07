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
``proxy --base-url URL``
    a request through the reverse proxy with a malformed model key:
    ``{"verdict": "trusted_proxy_unset"}`` (503 proxy_misconfigured — exit 1)
    or ``{"verdict": "trusted_proxy_set"}`` (the door's 401 — exit 0);
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
    comma-separated keys in ``--consumers-keys-env`` (8 consumers live),
    while sampling ``--mem-file`` (a cgroup ``memory.current`` of the ctx
    container, or a file a sampler rewrites) every 50 ms from before the
    flood to its end: ``{"base_bytes", "peak_bytes", "budget_bytes",
    "streams", "failed_streams", "samples", "ok"}``, ok = peak − base <
    budget.
``retry-after --base-url URL --key-env NAME --model M --requests N [--concurrency C]``
    N non-streamed chats (C at a time):
    ``{"requests", "status": {…}, "share_429", "retry_after": [...],
    "violations": [...], "ok"}`` — ok = every 429 carries a whole-second
    Retry-After in [1, 30] and an x-should-retry header.
"""

import argparse
import json
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
RETRY_AFTER_MIN, RETRY_AFTER_MAX = 1, 30


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

def proxy(base_url: str) -> tuple[bool, dict]:
    status, _, raw = http("POST", base_url.rstrip("/") + "/v1/chat/completions",
                          {"model": "probe", "messages": [{"role": "user", "content": "x"}]},
                          {"Authorization": "Bearer ctxm_proxy-probe-not-a-key"}, timeout=30)
    code = _code(raw)
    if status == 503 and code == "proxy_misconfigured":
        return False, {"verdict": "trusted_proxy_unset", "status": status, "code": code}
    if status == 401 and code == "invalid_api_key":
        return True, {"verdict": "trusted_proxy_set", "status": status, "code": code}
    if status == 404 and code == "not_found":
        return False, {"verdict": "gateway_off", "status": status, "code": code,
                       "note": "the door is behind gateway.enabled — probe right after the flip"}
    return False, {"verdict": "unexpected", "status": status, "code": code}


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
    cut = r["prefill_s"] is None
    res = {"tokens": tokens, "first_byte_s": r["first_byte_s"], "prefill_s": r["prefill_s"], "total_s": r["total_s"],
           "cut_off": cut, "status": r["status"], "error": r["error"]}
    return not cut, res


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

def retry_after(base_url: str, key: str, model: str, requests: int, concurrency: int) -> tuple[bool, dict]:
    body = {"model": model, "messages": [{"role": "user", "content": "hi"}], "max_tokens": 4}
    url = base_url.rstrip("/") + "/v1/chat/completions"

    def one(_):
        st, hdr, _raw = http("POST", url, body, {"Authorization": f"Bearer {key}"})
        return st, hdr.get("Retry-After"), hdr.get("x-should-retry")

    with ThreadPoolExecutor(max_workers=max(1, concurrency)) as pool:
        got = list(pool.map(one, range(requests)))
    status: dict[str, int] = {}
    values, violations = [], []
    for st, ra, sr in got:
        status[str(st)] = status.get(str(st), 0) + 1
        if st != 429:
            continue
        if sr not in ("true", "false"):
            violations.append(f"429 without x-should-retry ({sr!r})")
        try:
            n = int(ra)
        except (TypeError, ValueError):
            violations.append(f"429 with Retry-After {ra!r}")
            continue
        values.append(n)
        if not RETRY_AFTER_MIN <= n <= RETRY_AFTER_MAX:
            violations.append(f"429 with Retry-After {n} outside [{RETRY_AFTER_MIN}, {RETRY_AFTER_MAX}]")
    n429 = status.get("429", 0)
    ok = not violations
    return ok, {"requests": requests, "status": status, "share_429": n429 / requests if requests else 0.0,
                "retry_after": sorted(set(values)), "violations": violations[:10], "ok": ok}


# ── CLI ─────────────────────────────────────────────────────────────────────

def _parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="measure.py", description=__doc__.splitlines()[0])
    p.add_argument("--list", action="store_true", help="print the implemented subcommand ids")
    sub = p.add_subparsers(dest="cmd")
    px = sub.add_parser("proxy")
    px.add_argument("--base-url", required=True)
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
            ok, res = proxy(args.base_url)
        elif args.cmd == "prefill":
            ok, res = prefill(args.base_url, _env(args.key_env), args.model, args.tokens)
        elif args.cmd == "heap":
            keys = [k.strip() for k in _env(args.consumers_keys_env).split(",") if k.strip()]
            ok, res = heap(args.base_url, keys, args.model, args.streams, args.mem_file, args.budget_bytes)
        else:
            ok, res = retry_after(args.base_url, _env(args.key_env), args.model, args.requests, args.concurrency)
    except (OSError, ValueError) as e:
        ok, res = False, {"error": f"{type(e).__name__}: {e}"}
    print(json.dumps({"measure": args.cmd, **res}))
    return EXIT_PASS if ok else EXIT_FAIL


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
