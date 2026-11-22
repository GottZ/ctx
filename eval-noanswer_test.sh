#!/usr/bin/env bash
#
# Gate for the run rules of eval.sh and eval-temporal.sh (#472).
#
# Usage: bash eval-noanswer_test.sh
#
# Runs copies of eval.sh and eval-temporal.sh in a temporary directory, next to
# a throwaway .env (a fake key, never a real one), against servers this test
# starts on 127.0.0.1: nothing leaves the host and no ctx server is needed.
# eval-temporal.sh reads its test blocks with `docker exec … psql`; here it runs
# with CTX_DB_CONTAINER set to a name no container carries and a `docker` shim
# first on PATH that answers `exec` with two canned rows and refuses anything
# else — no real docker command runs. The copies differ from the scripts in
# their request timeouts (the *_TIMEOUT_S lines are lowered to 1 s; each has to
# match exactly once, or the timeout cases fail). Needs python3 and curl.
#
# Pinned:
#   0. eval_no_answer_class (scripts/lib/eval-api.sh): a request got an answer
#      only with curl exit 0, a 2xx status and a JSON object with success:true;
#      otherwise its class is timeout, connection, http_401_403, http_5xx,
#      http_other or bad_body. With ANSWER ok, ok:true; with jsonrpc, a
#      result object without error and without isError:true (#478).
#   1. eval.sh, every request answered: rc 0, results, baseline and diff written.
#   2. eval.sh, no answer in the preflight (refused connection, HTTP 500, a
#      timeout): rc 1, nothing written, the reason named. HTTP 401, in the
#      preflight or at the first test: rc 1 at once, nothing written,
#      CONTEXT_API_KEY_PRIVATE named, the key not. A ~/.curlrc with location
#      and fail changes neither: 401 still stops, a redirect is not followed.
#      (Every server here answers 401 to a request without the throwaway key
#      in X-Context-Key, so every case also pins that the key arrives.)
#   3. eval.sh, no test answered (HTTP 500, a timeout): rc 1, nothing written,
#      "no test got an answer".
#   4. eval.sh, some tests without an answer: those tests are NOT_MEASURED —
#      neither a pass nor a fail (an unanswered negative case used to pass):
#      results written with not_measured, rc 3, no baseline and no diff even
#      with --update-baseline, and the last line says why. An old baseline (no
#      not_measured field) stays readable in the comparison.
#   5. eval-temporal.sh, every request answered: rc 0, the CSV under
#      .eval-results with mode 0600 (not under /tmp), or at EVAL_TEMPORAL_RESULTS;
#      an EVAL_TEMPORAL_RESULTS that cannot be written stops before a request.
#   6. eval-temporal.sh, no request answered (refused, HTTP 500, HTTP 401):
#      rc 1, no CSV, no analysis; 401 after the first request, naming
#      CONTEXT_API_KEY_PRIVATE.
#   7. eval-temporal.sh, some requests without an answer (sources that is no
#      list among them): rc 1, the CSV marks them in no_answer (target_found
#      empty, never False), the analysis rates the answered ones and says how
#      many got none. A real miss stays False.

set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

python3 -I - "$TEST_DIR" <<'PY'
import csv
import http.server
import json
import os
import re
import shutil
import socket
import stat
import subprocess
import sys
import tempfile
import threading
import time

REPO = sys.argv[1]
KEY = "eval-noanswer-test-throwaway-key"
# A hanging server sleeps this long before it gives up on its request:
# longer than the copies' 1-s timeouts.
HANG_S = 3
HANG = "hang"
EVAL_TIMEOUTS = ("QUERY_TIMEOUT_S=120", "SEARCH_TIMEOUT_S=30", "STATS_TIMEOUT_S=15", "CENSUS_TIMEOUT_S=60")
TEMPORAL_TIMEOUTS = ("QUERY_TIMEOUT_S=120", "SEARCH_TIMEOUT_S=30")
REAL_CURL = shutil.which("curl") or "curl"
BLOCK1 = "aaaaaaaa-0000-0000-0000-000000000001"
BLOCK2 = "aaaaaaaa-0000-0000-0000-000000000002"
REFUSAL = "I don't know based on the available sources."

passed = failed = 0


def check(ok, name, detail=""):
    global passed, failed
    if ok:
        passed += 1
        print(f"[OK]   {name}")
    else:
        failed += 1
        print(f"[FAIL] {name}  {detail}"[:900])


def body(doc):
    return json.dumps(doc).encode()


# --- the eval.sh case table, read off the script: an answer that carries every
# keyword passes every positive case, the refusal every negative one.
CASES = []
with open(os.path.join(REPO, "eval.sh")) as f:
    for line in f:
        m = re.match(r"^([A-Z][0-9]+)\|(retrieval|synthesis)\|([^|]*)\|([^|]*)\|([^|]*)\|", line)
        if m:
            CASES.append(dict(zip(("id", "type", "query", "conf", "keywords"), m.groups())))
QUERY_OF = {c["id"]: c["query"] for c in CASES}
NEGATIVE = {c["query"] for c in CASES if c["type"] == "synthesis" and c["conf"] == "none"}
WORDS = " ".join(alt for c in CASES for group in c["keywords"].split(",") for alt in group.split("~") if alt)
N_TESTS = len(CASES)
STATS = (200, body({"success": True, "action": "stats", "stats": {"total_blocks": 500}}))
SEARCH = (200, body({"success": True, "count": 1, "results": [{"id": "b1", "title": WORDS, "content_preview": ""}]}))
ANSWER = (200, body({"success": True, "answer": "Answer: " + WORDS, "confidence": "confident",
                     "sources": [{"id": "b1", "title": "t"}]}))
REFUSED = (200, body({"success": True, "answer": REFUSAL, "confidence": "no_relevant_blocks_found", "sources": []}))


def status(code):
    return (code, body({"success": False, "error": f"status {code}"}))


def healthy(path, query, n):
    if path == "/api/manage":
        return STATS
    if path == "/api/search":
        return SEARCH
    return REFUSED if query in NEGATIVE else ANSWER


class Server:
    """A local server on 127.0.0.1: answer(path, query, n) -> (status, body),
    (status, body, headers) or HANG (sleeps past the client's timeout, then
    closes). A request without the throwaway key in X-Context-Key gets 401."""

    def __init__(self, answer):
        self.requests = 0
        self.lock = threading.Lock()
        outer = self

        class H(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_POST(self):
                with outer.lock:
                    outer.requests += 1
                    n = outer.requests
                length = int(self.headers.get("Content-Length") or 0)
                try:
                    query = json.loads(self.rfile.read(length) or b"{}").get("query")
                except ValueError:
                    query = None
                if self.headers.get("X-Context-Key") != KEY:
                    got = (401, body({"error": "unauthorized"}))
                else:
                    got = answer(self.path, query, n)
                if got == HANG:
                    time.sleep(HANG_S)
                    return
                code, payload, headers = (got + ({},))[:3]
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                for k, v in headers.items():
                    self.send_header(k, v)
                self.end_headers()
                self.wfile.write(payload)

        self.srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.srv.daemon_threads = True
        self.srv.block_on_close = False
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


SHIM = f"""#!/usr/bin/env bash
if [[ "$1" == exec ]]; then
  printf '%s\\n' '{BLOCK1}|probe block one|learnings|2026-10-01|first probe block' \\
                 '{BLOCK2}|probe block two|decisions|2026-09-15|second probe block'
  exit 0
fi
echo "docker shim: refused $*" >&2
exit 1
"""


def lowered(src, lines):
    """The script with each timeout line lowered to 1 s, and whether every one
    of them matched exactly once."""
    ok = True
    for line in lines:
        name = line.split("=")[0]
        if src.count("\n" + line + "\n") != 1:
            ok = False
            continue
        src = src.replace("\n" + line + "\n", f"\n{name}=1\n")
    return src, ok


class Copy:
    """A directory with copies of the two scripts, scripts/lib and a throwaway
    .env pointing at a local port."""

    def __init__(self, work, port, curlrc=""):
        self.dir = tempfile.mkdtemp(dir=work)
        # CURL_HOME is this directory: no ~/.curlrc of whoever runs the test,
        # or the one a case writes here.
        with open(os.path.join(self.dir, ".curlrc"), "w") as f:
            f.write(curlrc)
        os.makedirs(os.path.join(self.dir, "scripts", "lib"))
        for lib in ("db-container.sh", "eval-api.sh"):
            src = os.path.join(REPO, "scripts", "lib", lib)
            if os.path.exists(src):
                shutil.copyfile(src, os.path.join(self.dir, "scripts", "lib", lib))
        with open(os.path.join(REPO, "eval.sh")) as f:
            src, self.eval_lowered = lowered(f.read(), EVAL_TIMEOUTS)
        with open(os.path.join(self.dir, "eval.sh"), "w") as f:
            f.write(src)
        with open(os.path.join(REPO, "eval-temporal.sh")) as f:
            src, self.temporal_lowered = lowered(f.read(), TEMPORAL_TIMEOUTS)
        # A script that still writes under /tmp writes into this directory
        # instead (the static check in 5 holds the rule).
        self.tmpdir = os.path.join(self.dir, "tmp-redirect")
        os.makedirs(self.tmpdir)
        src = src.replace('RESULTS_FILE="/tmp/', f'RESULTS_FILE="{self.tmpdir}/')
        with open(os.path.join(self.dir, "eval-temporal.sh"), "w") as f:
            f.write(src)
        shim = os.path.join(self.dir, "shim")
        os.makedirs(shim)
        with open(os.path.join(shim, "docker"), "w") as f:
            f.write(SHIM)
        os.chmod(os.path.join(shim, "docker"), 0o700)
        self.shim = shim
        with open(os.path.join(self.dir, ".env"), "w") as f:
            f.write(f"WEBHOOK_BASE_URL=http://127.0.0.1:{port}\nCONTEXT_API_KEY_PRIVATE={KEY}\n"
                    "CONTEXT_DB_PASSWORD=eval-noanswer-test-not-a-password\n")

    def run(self, script, args, env_extra=None, argv_log=None):
        env = {k: v for k, v in os.environ.items()
               if not k.lower().endswith("_proxy")
               and not k.startswith(("CTX_", "CONTEXT_", "WEBHOOK_", "EVAL_", "OLLAMA_"))}
        env["PATH"] = self.shim + ":" + env["PATH"]
        if argv_log is not None:
            # A curl first on PATH that writes its arguments to argv_log and
            # runs the real one: what any local user sees in the process list.
            spy = os.path.join(self.dir, "spy")
            os.makedirs(spy)
            with open(os.path.join(spy, "curl"), "w") as f:
                f.write(f'#!/usr/bin/env bash\nprintf "%s\\n" "$@" >> "{argv_log}"\nexec "{REAL_CURL}" "$@"\n')
            os.chmod(os.path.join(spy, "curl"), 0o700)
            env["PATH"] = spy + ":" + env["PATH"]
        env["CTX_DB_CONTAINER"] = "eval-noanswer-test-no-such-container"
        env["CURL_HOME"] = self.dir
        env.update(env_extra or {})
        return subprocess.run(["bash", os.path.join(self.dir, script)] + args, capture_output=True,
                              text=True, env=env, timeout=300, cwd=self.dir)

    def written(self):
        """Every file a run left behind: results, baseline, diff, CSVs."""
        out = []
        for root, _, files in os.walk(self.dir):
            for fn in files:
                rel = os.path.relpath(os.path.join(root, fn), self.dir)
                if rel.startswith((".eval", "tmp-redirect", "out")):
                    out.append(rel)
        return sorted(out)

    def results(self):
        d = os.path.join(self.dir, ".eval-results")
        files = sorted(f for f in os.listdir(d) if f.startswith("eval-results-")) if os.path.isdir(d) else []
        if not files:
            return None
        with open(os.path.join(d, files[-1])) as f:
            return json.load(f)

    def csvs(self):
        return [p for p in self.written() if "eval-temporal" in p or p.startswith("out")]


def say(p):
    out = (p.stdout + p.stderr).strip().splitlines()
    return f"rc={p.returncode} last={out[-3:]!r}"


def last_line(p):
    lines = p.stdout.rstrip("\n").splitlines()
    return lines[-1] if lines else ""


def classify(rc, code, text, *answer):
    lib = os.path.join(REPO, "scripts", "lib", "eval-api.sh")
    p = subprocess.run(["bash", "-c", 'source "$1" && shift && eval_no_answer_class "$@"', "_", lib,
                        str(rc), code, text, *answer], capture_output=True, text=True)
    return p.stdout.strip() if p.returncode == 0 else f"<rc {p.returncode}: {p.stderr.strip()[:120]}>"


def main(work):
    print("--- 0. eval_no_answer_class")
    for rc, code, text, want in [
        (0, "200", '{"success":true,"answer":"a"}', ""),
        (0, "200", '   {"success":true}', ""),
        (0, "201", '{"success":true}', ""),
        (7, "000", "", "connection"),
        (6, "000", "", "connection"),
        (60, "000", "", "connection"),
        (52, "000", "", "connection"),
        (28, "000", "", "timeout"),
        (28, "200", "     ", "timeout"),
        (0, "401", '{"success":false,"error":"unauthorized"}', "http_401_403"),
        (0, "403", '{"success":false,"error":"admin key required"}', "http_401_403"),
        (0, "500", '{"success":false,"error":"boom"}', "http_5xx"),
        (0, "503", "", "http_5xx"),
        (0, "404", "not found", "http_other"),
        (0, "302", "", "http_other"),
        (0, "200", "<html>a proxy page</html>", "bad_body"),
        (0, "200", "[]", "bad_body"),
        (0, "200", "", "bad_body"),
        (0, "200", '{"status":"ok"}', "bad_body"),
        (0, "200", '{"success":false,"error":"synthesis failed"}', "bad_body"),
        (0, "200", '{"success":"true"}', "bad_body"),
    ]:
        got = classify(rc, code, text)
        check(got == want, f"curl {rc}, HTTP {code}, body {text[:30]!r}: {want or 'an answer'}", f"got {got!r}")
    # The ANSWER shapes test.sh uses (#478): ok for /api/synthesize/daily,
    # jsonrpc for /mcp; an unknown shape is never met.
    for answer, text, want in [
        ("success", '{"success":true}', ""),
        ("ok", '{"ok":true,"block_id":"b"}', ""),
        ("ok", '{"ok":false,"error":"synthesis failed"}', "bad_body"),
        ("ok", '{"success":true}', "bad_body"),
        ("jsonrpc", '{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}', ""),
        ("jsonrpc", '{"jsonrpc":"2.0","id":2,"result":{"content":[],"isError":true}}', "bad_body"),
        ("jsonrpc", '{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"m"}}', "bad_body"),
        ("jsonrpc", '{"jsonrpc":"2.0","id":1}', "bad_body"),
        ("jsonrpc", '{"jsonrpc":"2.0","id":1,"result":[]}', "bad_body"),
        ("no-such-shape", '{"success":true}', "bad_body"),
    ]:
        got = classify(0, "200", text, answer)
        check(got == want, f"answer {answer}, body {text[:40]!r}: {want or 'an answer'}", f"got {got!r}")

    print("--- 1. eval.sh: every request answered")
    s = Server(healthy)
    c = Copy(work, s.port)
    argv_log = os.path.join(c.dir, "curl-argv.log")
    p = c.run("eval.sh", ["--no-warmup", "--update-baseline"], argv_log=argv_log)
    s.close()
    res = c.results()
    check(p.returncode == 0 and res is not None and ".eval-baseline.json" in c.written()
          and ".eval-baseline.diff" in c.written(), "rc 0, results, baseline and diff written", say(p))
    argv = open(argv_log).read() if os.path.exists(argv_log) else ""
    check(argv.count("--max-time") == N_TESTS + 2 and KEY not in argv,
          "the key is on no curl command line (each of the 49 requests)",
          f"requests={argv.count('--max-time')} key_on_command_line={KEY in argv}")
    check(res is not None and res["summary"]["passed"] == N_TESTS and (res.get("not_measured") or {}).get("count") == 0,
          f"results: {N_TESTS}/{N_TESTS} passed, not_measured 0",
          json.dumps({k: res.get(k) for k in ("summary", "not_measured")})[:300] if res else "no results")
    old_baseline = None
    if res is not None:
        # The shape of a baseline written before #472: no not_measured field.
        old_baseline = {k: v for k, v in res.items() if k != "not_measured"}

    print("--- 2. eval.sh: no answer in the preflight, or HTTP 401")
    for name, answer, want in [
        ("a refused connection", None, "connection"),
        ("HTTP 500", lambda path, q, n: status(500), "HTTP 500"),
        ("a timeout", lambda path, q, n: HANG, "timeout"),
    ]:
        s = Server(answer) if answer else None
        c = Copy(work, s.port if s else free_port())
        if name == "a timeout" and not c.eval_lowered:
            check(False, f"{name}: rc 1, nothing written, the reason named",
                  "eval.sh carries no lines " + ", ".join(EVAL_TIMEOUTS) + " (each exactly once) to lower")
            s.close()
            continue
        p = c.run("eval.sh", ["--no-warmup", "--update-baseline"])
        if s:
            s.close()
        out = p.stdout + p.stderr
        check(p.returncode == 1 and c.written() == [] and "no answer" in out and want in out
              and "only 0 blocks" not in out,
              f"{name}: rc 1, nothing written, the reason named ({want})", say(p) + f" written={c.written()}")
    for name, answer, n_requests in [
        ("HTTP 401 in the preflight", lambda path, q, n: status(401), 1),
        ("HTTP 401 at the first test", lambda path, q, n: STATS if path == "/api/manage" else status(401), 3),
    ]:
        s = Server(answer)
        c = Copy(work, s.port)
        p = c.run("eval.sh", ["--no-warmup", "--update-baseline"])
        s.close()
        out = p.stdout + p.stderr
        check(p.returncode == 1 and c.written() == [] and s.requests == n_requests,
              f"{name}: rc 1 after request {n_requests}, nothing written",
              say(p) + f" requests={s.requests} written={c.written()}")
        check("CONTEXT_API_KEY_PRIVATE" in out and "401" in out and KEY not in out,
              f"{name}: names CONTEXT_API_KEY_PRIVATE, not the key", say(p))
    # A ~/.curlrc with `location` (follow redirects) and `fail` (a status is a
    # curl error) changes neither rule: the key goes to the base URL alone, and
    # 401 is still recognised.
    hostile = "location\nfail\n"
    s = Server(lambda path, q, n: status(401))
    c = Copy(work, s.port, curlrc=hostile)
    p = c.run("eval.sh", ["--no-warmup"])
    s.close()
    out = p.stdout + p.stderr
    check(p.returncode == 1 and c.written() == [] and "CONTEXT_API_KEY_PRIVATE" in out and "401" in out,
          "a ~/.curlrc with location and fail: HTTP 401 still stops naming CONTEXT_API_KEY_PRIVATE", say(p))
    target = Server(healthy)
    s = Server(lambda path, q, n: (302, b"", {"Location": f"http://127.0.0.1:{target.port}{path}"}))
    c = Copy(work, s.port, curlrc=hostile)
    p = c.run("eval.sh", ["--no-warmup"])
    s.close()
    target.close()
    out = p.stdout + p.stderr
    check(p.returncode == 1 and c.written() == [] and target.requests == 0 and "HTTP 302" in out,
          "a ~/.curlrc with location and fail: a redirect is not followed (its target got no request)",
          say(p) + f" target={target.requests}")

    print("--- 3. eval.sh: the preflight answers, no test does")
    for name, answer, args, want in [
        ("HTTP 500", lambda path, q, n: STATS if path == "/api/manage" else status(500),
         ["--no-warmup", "--update-baseline"], f"{N_TESTS} of {N_TESTS}"),
        ("a timeout", lambda path, q, n: STATS if path == "/api/manage" else HANG,
         ["--no-warmup", "--retrieval-only", "--update-baseline"], "timeout"),
    ]:
        s = Server(answer)
        c = Copy(work, s.port)
        if name == "a timeout" and not c.eval_lowered:
            check(False, f"{name}: rc 1, nothing written, said so", "eval.sh carries no timeout lines to lower")
            s.close()
            continue
        p = c.run("eval.sh", args)
        s.close()
        out = p.stdout + p.stderr
        check(p.returncode == 1 and c.written() == [] and "no test got an answer" in out and want in out,
              f"{name}: rc 1, nothing written, 'no test got an answer' ({want})",
              say(p) + f" written={c.written()}")

    print("--- 4. eval.sh: some tests without an answer")
    # Five tests get no answer; N02 gets a real answer that fails (a confident
    # answer to an off-topic question: a false positive), so the rates show
    # whose denominator they use.
    fails = {QUERY_OF["S01"]: status(500), QUERY_OF["N01"]: status(500), QUERY_OF["I01"]: status(503),
             QUERY_OF["K01"]: (200, body({"success": False, "error": "synthesis failed", "confidence": "confident"})),
             QUERY_OF["R02"]: HANG,
             QUERY_OF["N02"]: (200, body({"success": True, "answer": "Install Istio with its operator.",
                                          "confidence": "confident", "sources": [{"id": "b1", "title": "t"}]}))}
    want_ids = {"S01": "http_5xx", "N01": "http_5xx", "I01": "http_5xx", "K01": "bad_body", "R02": "timeout"}
    n_synthesis = sum(1 for c in CASES if c["type"] == "synthesis")
    n_retrieval = N_TESTS - n_synthesis

    def partial(path, query, n):
        return fails.get(query) or healthy(path, query, n)

    for flag in (True, False):
        label = "with --update-baseline" if flag else "against an old baseline"
        s = Server(partial)
        c = Copy(work, s.port)
        if not c.eval_lowered:
            check(False, f"{label}: rc 3", "eval.sh carries no timeout lines to lower")
            s.close()
            continue
        base_path = os.path.join(c.dir, ".eval-baseline.json")
        if not flag and old_baseline is not None:
            with open(base_path, "w") as f:
                json.dump(old_baseline, f, indent=2)
        before = open(base_path).read() if os.path.exists(base_path) else None
        p = c.run("eval.sh", ["--no-warmup"] + (["--update-baseline"] if flag else []))
        s.close()
        res = c.results()
        by_id = {r["id"]: r for r in res["results"]} if res else {}
        nm = (res or {}).get("not_measured") or {}
        out = p.stdout + p.stderr
        check(p.returncode == 3 and res is not None, f"{label}: rc 3, results written", say(p))
        check({i: by_id.get(i, {}).get("verdict") for i in want_ids} == {i: "NOT_MEASURED" for i in want_ids}
              and {i: by_id.get(i, {}).get("no_answer") for i in want_ids} == want_ids,
              f"{label}: the five unanswered tests are NOT_MEASURED with their class (N01 is no pass)",
              json.dumps({i: [by_id.get(i, {}).get("verdict"), by_id.get(i, {}).get("no_answer")] for i in want_ids}))
        summary = (res or {}).get("summary") or {}
        check(res is not None and nm.get("count") == 5 and nm.get("by_class") == {"timeout": 1, "http_5xx": 3, "bad_body": 1}
              and summary.get("passed") == N_TESTS - 6 and summary.get("failed") == 1
              and summary.get("total") == N_TESTS and summary.get("pass_rate") == round((N_TESTS - 6) / (N_TESTS - 5) * 100, 1)
              and by_id.get("N02", {}).get("verdict") == "FAIL",
              f"{label}: not_measured 5 by class; {N_TESTS - 6} passed, 1 failed (N02), the rate over the measured",
              json.dumps({"nm": nm, "summary": summary})[:400])
        check(res is not None and summary.get("false_positive_rate") == 25.0
              and res["by_category"]["negative"] == {"pass": 3, "fail": 1, "total": 4}
              and nm.get("by_category", {}).get("negative") == 1,
              f"{label}: false positives 1 of the 4 measured negative tests; by_category counts the measured",
              json.dumps([summary.get("false_positive_rate"), (res or {}).get("by_category", {}).get("negative"),
                          nm.get("by_category")]))
        by_type = (res or {}).get("by_type") or {}
        check(by_type.get("synthesis", {}).get("total") == n_synthesis - 4
              and by_type.get("retrieval", {}).get("total") == n_retrieval - 1,
              f"{label}: by_type (counts and latencies) holds the measured tests alone",
              json.dumps({t: v.get("total") for t, v in by_type.items()}))
        # The summary and the regression check run with stderr silenced: a
        # crash shows as missing lines, so the lines after each new one are
        # asserted too.
        check("NOT MEASURED (" in p.stdout and f"{N_TESTS - 6}/{N_TESTS - 5} measured tests passed" in p.stdout,
              f"{label}: the verdict says NOT MEASURED and counts the measured", say(p))
        check("  Not measured:       5 (no answer:" in p.stdout and "  Category        Pass  Fail" in p.stdout
              and "  NOT MEASURED (no answer):" in p.stdout and "    -- N01: " in p.stdout
              and "    -- R02: " in p.stdout and "  search-endpt = the five" in p.stdout,
              f"{label}: the summary counts and lists the unanswered tests", p.stdout[-1200:])
        if flag:
            check(".eval-baseline.json" not in c.written() and ".eval-baseline.diff" not in c.written(),
                  f"{label}: no baseline, no diff", f"written={c.written()}")
            check("Baseline not written" in last_line(p) and "5 of " + str(N_TESTS) in last_line(p),
                  f"{label}: the last line says why no baseline was written", repr(last_line(p)))
        else:
            check(before is not None and open(base_path).read() == before, f"{label}: the baseline is untouched")
            check("Regression Check" in p.stdout and f"NOTE: 5 of {N_TESTS} tests got no answer" in p.stdout
                  and "  Baseline: " in p.stdout,
                  f"{label}: the comparison reads the old baseline and says what it compares", p.stdout[-900:])

    print("--- 5. eval-temporal.sh: every request answered")
    with open(os.path.join(REPO, "eval-temporal.sh")) as f:
        check("/tmp/" not in f.read(), "eval-temporal.sh names no path under /tmp")

    def found(path, query, n):
        return (200, body({"success": True, "answer": "a", "confidence": "confident",
                           "sources": [{"id": BLOCK2}, {"id": BLOCK1}]}))

    s = Server(found)
    c = Copy(work, s.port)
    p = c.run("eval-temporal.sh", [])
    s.close()
    csvs = c.csvs()
    mode = stat.S_IMODE(os.stat(os.path.join(c.dir, csvs[0])).st_mode) if len(csvs) == 1 else None
    check(p.returncode == 0 and len(csvs) == 1 and csvs[0].startswith(".eval-results/eval-temporal-")
          and mode == 0o600 and "Overall: 18/18 found" in p.stdout,
          "rc 0, one CSV under .eval-results, mode 0600, 18/18 found",
          say(p) + f" csvs={csvs} mode={oct(mode) if mode is not None else None}")
    s = Server(found)
    c = Copy(work, s.port)
    target = os.path.join(c.dir, "out", "temporal.csv")
    os.makedirs(os.path.dirname(target))
    p = c.run("eval-temporal.sh", [], {"EVAL_TEMPORAL_RESULTS": target})
    s.close()
    check(p.returncode == 0 and c.csvs() == ["out/temporal.csv"]
          and stat.S_IMODE(os.stat(target).st_mode) == 0o600 if os.path.exists(target) else False,
          "EVAL_TEMPORAL_RESULTS names the CSV (mode 0600), nothing else is written",
          say(p) + f" csvs={c.csvs()}")
    s = Server(found)
    c = Copy(work, s.port)
    p = c.run("eval-temporal.sh", [], {"EVAL_TEMPORAL_RESULTS": os.path.join(c.dir, "no-such-dir", "t.csv")})
    s.close()
    check(p.returncode == 1 and s.requests == 0 and c.csvs() == [] and "EVAL_TEMPORAL_RESULTS" in p.stderr,
          "an EVAL_TEMPORAL_RESULTS that cannot be written stops the run before its first request",
          say(p) + f" requests={s.requests}")

    print("--- 6. eval-temporal.sh: no request answered")
    for name, answer, n_requests, want in [
        ("a refused connection", None, None, "connection"),
        ("HTTP 500", lambda path, q, n: status(500), 18, "HTTP 500"),
        ("HTTP 401", lambda path, q, n: status(401), 1, "CONTEXT_API_KEY_PRIVATE"),
    ]:
        s = Server(answer) if answer else None
        c = Copy(work, s.port if s else free_port())
        p = c.run("eval-temporal.sh", [])
        if s:
            s.close()
        out = p.stdout + p.stderr
        check(p.returncode == 1 and c.csvs() == [] and "ANALYSIS" not in p.stdout and want in out
              and (n_requests is None or s.requests == n_requests) and KEY not in out,
              f"{name}: rc 1, no CSV, no analysis, the reason named ({want})",
              say(p) + f" csvs={c.csvs()} requests={s.requests if s else '-'}")

    print("--- 7. eval-temporal.sh: some requests without an answer")

    def some(path, query, n):
        if n % 3 == 0:
            return status(500)
        if n == 4:
            return HANG
        if n == 5:
            # Answered, the target not among the sources: a real miss.
            return (200, body({"success": True, "answer": "a", "confidence": "confident", "sources": [{"id": "zz"}]}))
        if n == 7:
            # success:true, but sources is no list: no measurement either.
            return (200, body({"success": True, "answer": "a", "confidence": "confident", "sources": {"id": BLOCK1}}))
        return found(path, query, n)

    s = Server(some)
    c = Copy(work, s.port)
    if not c.temporal_lowered:
        check(False, "rc 1, the CSV marks the unanswered", "eval-temporal.sh carries no timeout lines to lower")
    else:
        p = c.run("eval-temporal.sh", [])
        csvs = c.csvs()
        rows = []
        if len(csvs) == 1:
            with open(os.path.join(c.dir, csvs[0])) as f:
                rows = list(csv.DictReader(f))
        marked = [r for r in rows if r.get("no_answer")]
        check(p.returncode == 1 and len(csvs) == 1 and len(rows) == 18, "rc 1, one CSV with 18 rows",
              say(p) + f" csvs={csvs} rows={len(rows)}")
        check(len(marked) == 8 and sorted({r["no_answer"] for r in marked}) == ["bad_body", "http_5xx", "timeout"]
              and all(r["target_found"] == "" for r in marked),
              "the 8 unanswered rows (sources no list among them) carry their class and no target_found",
              json.dumps([(r.get("test_id"), r.get("target_found"), r.get("no_answer")) for r in rows])[:400])
        check(sum(1 for r in rows if r.get("target_found") == "False") == 1,
              "the answered miss stays target_found False",
              json.dumps([(r.get("test_id"), r.get("target_found")) for r in rows])[:300])
        check("8 of 18" in p.stdout and "Overall: 9/10 found" in p.stdout and "ANALYSIS" in p.stdout,
              "the analysis rates the 10 answered and says 8 of 18 got none", p.stdout[-900:])
    s.close()


work = tempfile.mkdtemp(prefix="eval-noanswer-test.")
try:
    main(work)
finally:
    shutil.rmtree(work, ignore_errors=True)
print("")
print(f"  {passed} passed, {failed} failed")
sys.exit(1 if failed else 0)
PY
