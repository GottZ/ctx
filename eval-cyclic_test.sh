#!/usr/bin/env bash
#
# Gate for eval-cyclic.sh's run rules (#460).
#
# Usage: bash eval-cyclic_test.sh
#
# Runs a copy of eval-cyclic.sh in a temporary directory, next to a throwaway
# .env (a fake key, never a real one) and a three-case gold file, against
# servers this test starts on 127.0.0.1: nothing leaves the host and no ctx
# server is needed. The copy differs from eval-cyclic.sh in one line: its
# request timeout TIMEOUT_S is 1 s instead of 120 s (that line has to match
# exactly once, or the timeout case fails). Needs python3 and openssl.
#
# Pinned:
#   1. Every case answered: rc 0, results and (--baseline) baseline written,
#      n_errors 0.
#   2. No case answered (HTTP 500/503, a refused connection, a timeout, a 200
#      whose body is not JSON, a JSON list, an object without success:true,
#      success:false) or no case at all: rc 1, nothing written — a run
#      without one answer is no measurement — and only the reason on stderr.
#   3. HTTP 401: rc 1 after the first request, CONTEXT_API_KEY_PRIVATE named
#      (the key itself not), nothing written. A key with a CR (a CRLF .env)
#      does not reach any output either. A redirect stops the run (#445):
#      nothing written, its target gets no request.
#   4. One case of three without an answer: results with n_errors 1 and the
#      error on that case, rc 1, the summary names it, and no baseline even
#      with --baseline. A 200 with success:false is its error alone: none of
#      its other fields (sources of any shape, confidence, dimension weights)
#      reach the case.
#   4b. A case without an answer is neither a pass nor a miss: no verdict, no
#      overlap; per_bucket counts it in n and n_errors, and pass rate,
#      overlap, dim_weight_pass_rate and mean_pass_rate are over the cases
#      that got one (an unanswered N case used to pass, an unanswered C/M/L/F
#      case to fail).
#   5. A TLS certificate that does not verify stops the run with the advice
#      that fits it: self-signed → CTX_EVAL_INSECURE=1; a name the certificate
#      does not carry → WEBHOOK_BASE_URL, without CTX_EVAL_INSECURE; expired →
#      renew, without CTX_EVAL_INSECURE (SKIP where openssl cannot set the
#      validity dates, -not_before needs OpenSSL 3.4). A trusted one runs.
#   6. --help prints the leading comment block and stops before the code.

set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

python3 -I - "${TEST_DIR}/eval-cyclic.sh" <<'PY'
import http.server
import json
import os
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time

SCRIPT = sys.argv[1]
KEY = "eval-cyclic-test-throwaway-key"
CASES = [
    {"id": "c1", "bucket": "C", "query": "q one", "expected_top_k_block_ids": ["b1"]},
    {"id": "c2", "bucket": "M", "query": "q two", "expected_top_k_block_ids": ["b1"]},
    {"id": "c3", "bucket": "L", "query": "q three", "expected_top_k_block_ids": ["b1"]},
]
ANSWER = json.dumps({"success": True, "sources": [{"id": "b1"}], "confidence": "confident"}).encode()
TIMEOUT_LINE, TIMEOUT_TEST_LINE = "\nTIMEOUT_S = 120\n", "\nTIMEOUT_S = 1\n"
# A hanging server sleeps this long before it gives up on its request:
# longer than the copy's 1-s timeout.
HANG_S = 3

passed = failed = skipped = 0


def skip(name, why):
    global skipped
    skipped += 1
    print(f"[SKIP] {name}  {why}")


def check(ok, name, detail=""):
    global passed, failed
    if ok:
        passed += 1
        print(f"[OK]   {name}")
    else:
        failed += 1
        print(f"[FAIL] {name}  {detail}"[:600])


def reply(status, ctype, payload):
    return lambda query: (status, ctype, payload)


OK = reply(200, "application/json", ANSWER)
NOT_JSON = reply(200, "text/html", b"<html>a proxy page</html>")
JSON_LIST = reply(200, "application/json", b"[]")
# Another JSON service at the address: an object, but not ctx's answer.
FOREIGN_JSON = reply(200, "application/json", json.dumps({"status": "ok", "results": []}).encode())
UNSUCCESSFUL = reply(200, "application/json", json.dumps({"success": False, "error": "synthesis failed"}).encode())
# The quota refusal carries error_code, not error.
UNSUCCESSFUL_CODE = reply(200, "application/json", json.dumps({"success": False, "error_code": "quota_exceeded"}).encode())
HANG = reply(None, None, None)


def status(code):
    return reply(code, "application/json", json.dumps({"success": False, "error": f"status {code}"}).encode())


def second_case_500(query):
    return status(500)(query) if query == "q two" else OK(query)


# One answered and one unanswered case per kind of expectation: ids (C, M),
# none (N: an unanswered case passed vacuously, an answer without sources
# passes), dimension weights (F).
MIXED_CASES = [
    {"id": "c1", "bucket": "C", "query": "q c", "expected_top_k_block_ids": ["b1"]},
    {"id": "m1", "bucket": "M", "query": "q m", "expected_top_k_block_ids": ["b1"]},
    {"id": "n1", "bucket": "N", "query": "q n", "must_not_contain_block_ids": ["b9"]},
    {"id": "n2", "bucket": "N", "query": "q n2", "must_not_contain_block_ids": ["b9"]},
    {"id": "f1", "bucket": "F", "query": "q f", "expected_dim_weights": {"month": 1}},
    {"id": "f2", "bucket": "F", "query": "q f2", "expected_dim_weights": {"month": 1}},
]
ANSWER_DW = json.dumps({"success": True, "sources": [{"id": "b1"}], "confidence": "confident",
                        "activated_dim_weights": {"month": 0.5}}).encode()
ANSWER_EMPTY = json.dumps({"success": True, "sources": [], "confidence": "none"}).encode()


def mixed(query):
    if query in ("q m", "q n", "q f"):
        return status(500)(query)
    if query == "q f2":
        return (200, "application/json", ANSWER_DW)
    if query == "q n2":
        return (200, "application/json", ANSWER_EMPTY)
    return OK(query)


class Server:
    """One local server on 127.0.0.1: answer(query) -> (status, type, body);
    a None status hangs past the client's timeout."""

    def __init__(self, answer, tls=None, location=None):
        self.requests = 0
        outer = self

        class H(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_POST(self):
                outer.requests += 1
                n = int(self.headers.get("Content-Length") or 0)
                try:
                    query = json.loads(self.rfile.read(n) or b"{}").get("query")
                except ValueError:
                    query = None
                if location is not None:
                    self.send_response(302)
                    self.send_header("Location", location + self.path)
                    self.send_header("Content-Length", "0")
                    self.end_headers()
                    return
                code, ctype, payload = answer(query)
                if code is None:
                    time.sleep(HANG_S)
                    return
                self.send_response(code)
                self.send_header("Content-Type", ctype)
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

        self.srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.srv.daemon_threads = True
        self.srv.block_on_close = False
        if tls is not None:
            self.srv.socket = tls.wrap_socket(self.srv.socket, server_side=True)
        self.port = self.srv.server_address[1]
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()


def free_port():
    """A port on 127.0.0.1 nobody listens on: bound once, then released."""
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def run(work, copy, base_url, baseline=True, env_extra=None, args=None, cases=CASES, eol="\n"):
    d = tempfile.mkdtemp(dir=work)
    shutil.copyfile(copy, os.path.join(d, "eval-cyclic.sh"))
    with open(os.path.join(d, ".env"), "w", newline="") as f:
        f.write(f"WEBHOOK_BASE_URL={base_url}\nCONTEXT_API_KEY_PRIVATE={KEY}{eol}")
    gold = os.path.join(d, "gold.json")
    with open(gold, "w") as f:
        json.dump({"cases": cases}, f)
    env = {k: v for k, v in os.environ.items()
           if not k.lower().endswith("_proxy") and k not in ("CTX_EVAL_INSECURE", "SSL_CERT_FILE", "SSL_CERT_DIR")}
    env.update(env_extra or {})
    if args is None:
        args = ["--gold", gold, "--branch", "t"] + (["--baseline"] if baseline else [])
    p = subprocess.run(["bash", os.path.join(d, "eval-cyclic.sh")] + args,
                       capture_output=True, text=True, env=env, timeout=120)
    res_path = os.path.join(d, ".eval-cyclic-results-t.json")
    results = None
    if os.path.exists(res_path):
        with open(res_path) as f:
            results = json.load(f)
    return p, results, os.path.exists(os.path.join(d, ".eval-cyclic-baseline.json"))


def say(p):
    return f"rc={p.returncode} stderr={p.stderr.strip()[-300:]!r}"


def cert(work, name, *validity):
    c, k = os.path.join(work, name + ".pem"), os.path.join(work, name + ".key")
    subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", k, "-out", c,
                    "-subj", "/CN=localhost", "-addext", "subjectAltName=DNS:localhost", *validity],
                   check=True, capture_output=True)
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(c, k)
    return c, ctx


def main(work):
    with open(SCRIPT) as f:
        src = f.read()
    lowered = src.count(TIMEOUT_LINE) == 1
    copy = os.path.join(work, "eval-cyclic.sh")
    with open(copy, "w") as f:
        f.write(src.replace(TIMEOUT_LINE, TIMEOUT_TEST_LINE))

    print("--- 1. every case answered")
    s = Server(OK)
    p, res, base = run(work, copy, f"http://127.0.0.1:{s.port}")
    s.close()
    check(p.returncode == 0 and res is not None and base and s.requests == 3,
          "rc 0, results and baseline written, three requests", say(p))
    check(res is not None and res.get("n_errors") == 0 and res.get("mean_pass_rate") == 1.0,
          "results: n_errors 0, every case passes", json.dumps(res)[:300] if res else "no results")

    print("--- 2. no case answered: nothing written, rc 1")
    dead = [("HTTP 500", status(500)), ("HTTP 503", status(503)), ("a body that is not JSON", NOT_JSON),
            ("a JSON list", JSON_LIST), ("a JSON object without success:true", FOREIGN_JSON),
            ("200 with success:false", UNSUCCESSFUL), ("200 with success:false and error_code alone", UNSUCCESSFUL_CODE),
            ("a refused connection", None), ("a timeout", HANG)]
    for name, answer in dead:
        if answer is HANG and not lowered:
            check(False, f"{name}: rc 1, no results, no baseline",
                  "eval-cyclic.sh carries no line 'TIMEOUT_S = 120' (exactly once) to lower")
            continue
        s = Server(answer) if answer is not None else None
        port = s.port if s else free_port()
        p, res, base = run(work, copy, f"http://127.0.0.1:{port}")
        if s:
            s.close()
        check(p.returncode == 1 and res is None and not base and "no case got an answer" in p.stderr
              and "empty result" not in p.stderr,
              f"{name}: rc 1, no results, no baseline, said so (and only that)", say(p))
    s = Server(OK)
    p, res, base = run(work, copy, f"http://127.0.0.1:{s.port}", cases=[])
    s.close()
    check(p.returncode == 1 and res is None and not base and "holds no cases" in p.stderr and s.requests == 0,
          "a gold file without cases: rc 1, nothing written, said so", say(p))

    print("--- 3. HTTP 401 stops at once and names the variable")
    s = Server(status(401))
    p, res, base = run(work, copy, f"http://127.0.0.1:{s.port}")
    s.close()
    check(p.returncode == 1 and res is None and not base, "rc 1, no results, no baseline", say(p))
    check(s.requests == 1, "stopped after the first request", f"requests={s.requests}")
    check("CONTEXT_API_KEY_PRIVATE" in p.stderr and KEY not in p.stderr + p.stdout,
          "names CONTEXT_API_KEY_PRIVATE, not the key", say(p))
    s = Server(OK)
    p, res, base = run(work, copy, f"http://127.0.0.1:{s.port}", eol="\r\n")
    s.close()
    check(p.returncode == 1 and res is None and not base and KEY not in p.stderr + p.stdout,
          "a key with a CR (CRLF .env): rc 1, nothing written, the key in no output", say(p))
    target = Server(OK)
    s = Server(OK, location=f"http://127.0.0.1:{target.port}")
    p, res, base = run(work, copy, f"http://127.0.0.1:{s.port}")
    s.close()
    target.close()
    check(p.returncode == 1 and res is None and not base and target.requests == 0 and "refused a redirect" in p.stderr,
          "a redirect: rc 1, nothing written, its target got no request", f"target={target.requests} " + say(p))

    print("--- 4. one case of three without an answer")
    for flag in (True, False):
        s = Server(second_case_500)
        p, res, base = run(work, copy, f"http://127.0.0.1:{s.port}", baseline=flag)
        s.close()
        label = "with --baseline" if flag else "without --baseline"
        errors = {c["id"]: c.get("error") for c in res["case_results"]} if res else {}
        check(p.returncode == 1 and res is not None and not base,
              f"{label}: rc 1, results written, no baseline", say(p))
        check(res is not None and res.get("n_errors") == 1 and errors.get("c1") is None
              and errors.get("c3") is None and "500" in str(errors.get("c2")),
              f"{label}: n_errors 1, the error on case c2 alone", json.dumps(errors))
        check("errors=1" in p.stdout and "no answer: c2:" in p.stdout,
              f"{label}: the summary counts and names it", p.stdout[-300:])
        if flag:
            check("baseline" in p.stderr and "not written" in p.stderr, "says the baseline was not written", say(p))
    # A failed answer is an error and nothing else: none of its other fields
    # reach the case, whatever their shape.
    failed_answers = [
        ("sources [null]", {"success": False, "error": "synthesis failed", "sources": [None]}),
        ("a matching source", {"success": False, "error": "synthesis failed", "sources": [{"id": "b1"}],
                               "confidence": "confident", "activated_dim_weights": {"month": 1.0}}),
    ]
    for name, body in failed_answers:
        failed_answer = reply(200, "application/json", json.dumps(body).encode())
        s = Server(lambda query, a=failed_answer: a(query) if query == "q two" else OK(query))
        p, res, base = run(work, copy, f"http://127.0.0.1:{s.port}")
        s.close()
        c2 = next((c for c in res["case_results"] if c["id"] == "c2"), {}) if res else {}
        check(p.returncode == 1 and res is not None and not base and res.get("n_errors") == 1
              and c2.get("error") == "synthesis failed" and "Traceback" not in p.stderr,
              f"200 success:false with {name} on c2: rc 1, results with n_errors 1 and its error, no baseline", say(p))
        check(res is not None and c2.get("case_pass") is not True and not c2.get("top5")
              and c2.get("confidence") is None and c2.get("activated_dim_weights") is None,
              f"200 success:false with {name} on c2: none of its other fields reach the case", json.dumps(c2)[:300])

    print("--- 4b. a case without an answer is neither a pass nor a miss")
    s = Server(mixed)
    p, res, base = run(work, copy, f"http://127.0.0.1:{s.port}", cases=MIXED_CASES)
    s.close()
    pb = res.get("per_bucket", {}) if res else {}
    by_id = {c["id"]: c for c in res["case_results"]} if res else {}
    check(p.returncode == 1 and res is not None and not base and res.get("n_errors") == 3,
          "rc 1, results with n_errors 3, no baseline", say(p))
    check(pb.get("N", {}).get("passes") == 1 and pb.get("N", {}).get("pass_rate") == 1.0
          and by_id.get("n2", {}).get("case_pass") is True,
          "N: the unanswered case without expectations is no pass, the empty success:true answer is one",
          json.dumps(pb.get("N")))
    check(pb.get("M", {}).get("pass_rate") is None and pb.get("M", {}).get("top_k_overlap_at_5") is None,
          "M: the unanswered case is no miss (no pass rate, no overlap)", json.dumps(pb.get("M")))
    check(pb.get("F", {}).get("passes") == 1 and pb.get("F", {}).get("pass_rate") == 1.0
          and res.get("dim_weight_pass_rate") == 1.0,
          "F: the rates are over the answered case (pass rate, dim_weight_pass_rate 1.0)",
          json.dumps(pb.get("F")) + f" dwr={res.get('dim_weight_pass_rate') if res else None}")
    check(res is not None and res.get("mean_pass_rate") == 1.0 and pb.get("C", {}).get("pass_rate") == 1.0,
          "mean_pass_rate over the buckets with an answered case (C, N, F): 1.0", json.dumps(res.get("mean_pass_rate") if res else None))
    check([pb.get(b, {}).get("n_errors") for b in "CMLNF"] == [0, 1, 0, 1, 1]
          and [pb.get(b, {}).get("n") for b in "CMLNF"] == [1, 1, 0, 2, 2],
          "per_bucket counts every case in n and the unanswered ones in n_errors",
          json.dumps({b: [pb.get(b, {}).get("n"), pb.get(b, {}).get("n_errors")] for b in "CMLNF"}))
    check(all(by_id.get(i, {}).get(k, "absent") is None for i in ("n1", "m1", "f1")
              for k in ("case_pass", "top_k_pass", "must_not_pass", "dim_weight_pass", "top_k_overlap")),
          "per case: an unanswered case carries no verdict and no overlap",
          json.dumps({i: {k: by_id.get(i, {}).get(k) for k in ("case_pass", "top_k_pass", "must_not_pass",
                                                              "dim_weight_pass", "top_k_overlap")}
                      for i in ("n1", "m1", "f1")}))
    check("  N: n=2 errors=1 passes=1 pass_rate=1.0" in p.stdout,
          "the summary shows the errors per bucket", p.stdout[-400:])

    print("--- 5. TLS: the advice fits the reason")
    good, good_ctx = cert(work, "good", "-days", "2")
    try:
        old, old_ctx = cert(work, "expired", "-not_before", "20200101000000Z", "-not_after", "20200102000000Z")
    except subprocess.CalledProcessError as e:
        old = None
        skip("expired", f"openssl cannot set the validity dates: {e.stderr.decode(errors='replace').strip()[:160]}")
    tls = [
        ("self-signed", good_ctx, "localhost", {}, ["CTX_EVAL_INSECURE=1"], []),
        ("name not on the certificate", good_ctx, "127.0.0.1", {"SSL_CERT_FILE": good},
         ["WEBHOOK_BASE_URL"], ["CTX_EVAL_INSECURE"]),
    ]
    if old is not None:
        tls.append(("expired", old_ctx, "localhost", {"SSL_CERT_FILE": old}, ["renew"], ["CTX_EVAL_INSECURE"]))
    for name, ctx, host, env, want, unwanted in tls:
        s = Server(OK, ctx)
        p, res, base = run(work, copy, f"https://{host}:{s.port}", env_extra=env)
        s.close()
        check(p.returncode == 1 and res is None and not base and s.requests == 0
              and "does not verify" in p.stderr and all(w in p.stderr for w in want)
              and not any(u in p.stderr for u in unwanted),
              f"{name}: rc 1, nothing written, advice {want}, not {unwanted}", say(p))
    s = Server(OK, good_ctx)
    p, res, base = run(work, copy, f"https://localhost:{s.port}", env_extra={"SSL_CERT_FILE": good})
    s.close()
    check(p.returncode == 0 and res is not None and base, "trusted certificate: rc 0, written", say(p))

    print("--- 6. --help")
    p, _, _ = run(work, copy, "http://127.0.0.1:1", args=["--help"])
    lines = p.stdout.rstrip("\n").split("\n")
    check(p.returncode == 0 and "Usage:" in p.stdout and all(ln.startswith("#") for ln in lines)
          and not lines[0].startswith("#!") and p.stdout.endswith("# Source: https://github.com/GottZ/ctx\n"),
          "the leading comment block, without the shebang, ending at its last line", repr(lines[:1] + lines[-2:]))


work = tempfile.mkdtemp(prefix="eval-cyclic-test.")
try:
    main(work)
finally:
    shutil.rmtree(work, ignore_errors=True)
print("")
print(f"  {passed} passed, {failed} failed, {skipped} skipped")
sys.exit(1 if failed else 0)
PY
