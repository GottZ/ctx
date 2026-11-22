#!/usr/bin/env bash
#
# Gate for test.sh's rule on requests without an answer (#478).
#
# Usage: bash test-noanswer_test.sh
#
# Runs copies of test.sh in temporary directories, next to a throwaway .env
# (fake keys, never real ones), against servers this test starts on 127.0.0.1:
# nothing leaves the host and no ctx server is needed. Shims first on PATH
# stand in for the rest of the stack, and no real one of them runs: `docker`
# answers `inspect` and the `exec … psql` queries of T07/T19/T23 with canned
# rows, `ctx` answers `contract` with exit 0 (T07), `uv` exits 77 (T25 is a
# SKIP; smoke.py t25 has its own self-test, selftest_live.py). HOME and
# CURL_HOME are the copy's directory, so no ~/.config/ctx or ~/.curlrc of
# whoever runs the test is read. The copies differ from test.sh in one line:
# MCP_TIMEOUT_S=30 is lowered to 1 s for the hang case (it has to match
# exactly once, or that case fails). Needs bash, python3 and curl.
#
# The stub tells every request apart by its test and step (T05 step 2 is the
# search after the save). A run faults one set of them with one form and
# answers everything else as a healthy ctx would:
#   empty          the connection closes without a response (curl: empty reply)
#   500            HTTP 500 with the API's error body
#   401            HTTP 401 {"error":"unauthorized"}, as handler.Auth answers
#   success:false  HTTP 200 whose body says the call failed: success:false;
#                  ok:false for /api/synthesize/daily; a JSON-RPC result with
#                  isError:true for /mcp
#   foreign        the status a refusal test expects, with a proxy's HTML page
#                  instead of the API's error body (refusal steps only)
#   noblocks       guard-list answering success:true without a blocks list
#                  (T04 and T09 only)
#
# Pinned:
#   0. docker, ctx and uv resolve to the shims — on a TMPDIR mounted noexec the
#      shell would skip them and run the real programs, so the test stops
#      before any run of test.sh (set TMPDIR to a directory that allows exec).
#   1. Healthy stub, with and without --with-ollama: rc 0, every test OK (T25
#      SKIP), and no key on a curl command line.
#   2. Fewer than 100 blocks: the corpus-bound tests SKIP with their reason,
#      rc 0 — a SKIP stays for a prerequisite the store documents.
#   3. The corpus gate without an answer (each form): rc 1, the run stops before
#      any test — no OK, no FAIL, no corpus SKIP — and names the reason; on
#      401 CONTEXT_API_KEY_PRIVATE, never the key.
#   4. The first request of every test without an answer (each form): every
#      test that sends one FAILs naming it, rc 1 — T01 alone passes on the
#      401, which is the refusal it expects.
#   5. The follow-up requests without an answer, one step at a time (each
#      form): step 2 fails T05, T06, T19, T20, T21, T22 and T24; step 3 T05
#      (get), T20 (PUT), T22 (confirm) and T24; step 4 T05 (delete) and T20
#      (DELETE); rc 1. A guard-list answer without a blocks list fails T04
#      and T09.
#   6. The refusal steps (T01; the 403 of T20, T21, T24; T22's unconfirmed
#      downgrade; T24's classify backend; T19's two 404) answered with
#      another form or with the expected status and a proxy's page: each
#      FAILs, rc 1 — a refusal test checks the status and the error body.
#   7. test.sh sends no request outside eval_api_request (every request has a
#      --max-time), and /mcp hanging past MCP_TIMEOUT_S fails T17 and T18 as a
#      timeout.

set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

python3 -I - "$TEST_DIR" <<'PY'
import http.server
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
from concurrent.futures import ThreadPoolExecutor

REPO = sys.argv[1]
PRIV = "test-noanswer-private-throwaway-key"
ISO = "test-noanswer-isotest-throwaway-key"
ADMIN = "test-noanswer-admin-throwaway-key"
KEYS = (PRIV, ISO, ADMIN)
HANG_S = 3
REAL_CURL = shutil.which("curl") or "curl"
FOCUS = "bbbbbbbb-0000-7000-8000-000000000001"
PRIV_BLOCK = "bbbbbbbb-0000-7000-8000-000000000002"
FORMS = ("empty", "500", "401", "success:false")

# Every test that sends a request of its own, and the ones with follow-up steps.
HTTP_TESTS = ["T01", "T02", "T03", "T04", "T05", "T06", "T08", "T09", "T11", "T12", "T19", "T20", "T21",
              "T22", "T24", "T13", "T14", "T15", "T16", "T17", "T18", "T23"]
# Per follow-up step, the tests whose verdict rests on it (T05: 2 search,
# 3 get, 4 delete; T20: 2 read, 3 PUT, 4 DELETE; T22: 2 refusal, 3 confirm;
# T24: 2 admin status, 3 refusal; T06/T19/T21: 2).
LATER_STEPS = {2: ["T05", "T06", "T19", "T20", "T21", "T22", "T24"], 3: ["T05", "T20", "T22", "T24"],
               4: ["T05", "T20"]}
# The steps whose promise is a refusal, with the status ctx refuses them with.
REFUSAL_STEPS = {("T01", 1): 401, ("T20", 1): 403, ("T21", 1): 403, ("T24", 1): 403,
                 ("T22", 2): 400, ("T24", 3): 422, ("T19", 2): 404}
REFUSAL_TESTS = sorted({t for t, _ in REFUSAL_STEPS})
ALL_OLLAMA = ["T01", "T02", "T03", "T04", "T05", "T06", "T07", "T08", "T09", "T10", "T11", "T12", "T19",
              "T20", "T21", "T22", "T24", "T13", "T14", "T15", "T16", "T17", "T18", "T23"]
ALL_SYSTEM = [t for t in ALL_OLLAMA if t not in ("T13", "T14", "T15", "T16", "T17", "T18", "T23")]
CORPUS_BOUND = ["T02", "T10", "T19", "T13", "T14", "T23"]
PROXY_PAGE = (b"<html>\r\n<head><title>{code}</title></head>\r\n<body>\r\n<center><h1>{code}</h1></center>\r\n"
              b"<hr><center>openresty</center>\r\n</body>\r\n</html>\r\n")

passed = failed = 0


def check(ok, name, detail=""):
    global passed, failed
    if ok:
        passed += 1
        print(f"[OK]   {name}")
    else:
        failed += 1
        print(f"[FAIL] {name}  {detail}"[:1200])


def js(doc):
    # Compact, as Go's encoding/json writes it ("success":true, no blank).
    return json.dumps(doc, separators=(",", ":"), ensure_ascii=False).encode()


def house_error(code, msg):
    return (code, js({"success": False, "error": msg}))


class Stub:
    """A healthy ctx on 127.0.0.1 for test.sh, which faults the requests
    `pick(test, step)` selects with `form`."""

    def __init__(self, pick=None, form=None, total_blocks=500):
        self.pick = pick or (lambda t, s: False)
        self.form = form
        self.total_blocks = total_blocks
        self.lock = threading.Lock()
        self.stats_priv = 0
        self.upserts = 0
        self.titles = {}
        self.seen = []
        outer = self

        class H(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def _serve(self, method):
                n = int(self.headers.get("Content-Length") or 0)
                raw = self.rfile.read(n) if n else b""
                try:
                    doc = json.loads(raw) if raw else {}
                except ValueError:
                    doc = {}
                key = self.headers.get("X-Context-Key") or ""
                auth = self.headers.get("Authorization") or ""
                if auth.startswith("Bearer "):
                    key = auth[len("Bearer "):]
                url = urllib.parse.urlsplit(self.path)
                got = outer.answer(method, url.path, urllib.parse.parse_qs(url.query), key, doc)
                if got is None:
                    return  # the connection closes without a response
                if got == "hang":
                    time.sleep(HANG_S)
                    return
                code, payload, headers = (got + ({},))[:3]
                self.send_response(code)
                self.send_header("Content-Type", headers.pop("Content-Type", "application/json"))
                self.send_header("Content-Length", str(len(payload)))
                for k, v in headers.items():
                    self.send_header(k, v)
                self.end_headers()
                self.wfile.write(payload)

            def do_GET(self):
                self._serve("GET")

            def do_POST(self):
                self._serve("POST")

            def do_PUT(self):
                self._serve("PUT")

            def do_DELETE(self):
                self._serve("DELETE")

        self.srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        self.srv.daemon_threads = True
        self.srv.block_on_close = False
        self.port = self.srv.server_address[1]
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()

    # -- which test and step a request belongs to ------------------------------
    def classify(self, method, path, query, key, doc):
        action = doc.get("action") if isinstance(doc, dict) else None
        data = (doc.get("data") or {}) if isinstance(doc, dict) else {}
        title = str(doc.get("title") or "") if isinstance(doc, dict) else ""
        text = str(doc.get("query") or "") if isinstance(doc, dict) else ""
        if path == "/api/manage":
            if key not in KEYS:
                return "T01", 1
            if action == "stats":
                if key == PRIV:
                    with self.lock:
                        self.stats_priv += 1
                        n = self.stats_priv
                    return ("gate", 1) if n == 1 else ("T02", 1)
                return "T03", 1
            if action == "guard-list":
                return ("T04", 1) if key == ISO else ("T09", 1)
            if action == "get":
                return "T05", 3
            if action == "delete":
                return ("T05", 4) if self.titles.get(doc.get("id"), "").startswith("__benchmark_test_") else ("cleanup", 9)
            if action == "guard-stats":
                return "T08", 1
            if action == "dream-stats":
                return "T11", 1
            if action == "dream-review":
                return "T12", 1
            if action == "backend-list":
                return ("T21", 1) if key != ADMIN else ("T21", 2)
            if action == "update":
                return ("T22", 3) if data.get("confirm_sensitivity_downgrade") else ("T22", 2)
            if action == "blocks-audit-status":
                return ("T24", 1) if key != ADMIN else ("T24", 2)
            if action == "backend-create":
                return "T24", 3
            return "cleanup", 9
        if path == "/api/store":
            if title.startswith("__benchmark_test_"):
                return "T05", 1
            if title.startswith("__benchmark_upsert_"):
                with self.lock:
                    self.upserts += 1
                    n = self.upserts
                return "T06", min(n, 2)
            return "T22", 1
        if path == "/api/search":
            if text.startswith("__benchmark_test_"):
                return "T05", 2
            if text.startswith("__benchmark_upsert_"):
                return "T06", 3
            return "T13", 1
        if path == "/api/query":
            return {"How does the Write Guard work?": "T14", "Rezept fuer Kartoffelsuppe": "T15"}.get(text, "T16"), 1
        if path == "/api/synthesize/daily":
            return "T23", 1
        if path == "/api/graph/ego":
            return ("T19", 1) if key == PRIV else ("T19", 2)
        if path == "/api/settings":
            return "T20", 1
        if path.startswith("/api/settings/"):
            return "T20", {"GET": 2, "PUT": 3, "DELETE": 4}.get(method, 9)
        if path == "/mcp":
            return ("T17", 1) if doc.get("method") == "tools/list" else ("T18", 1)
        return "unknown", 9

    def answer(self, method, path, query, key, doc):
        test, step = self.classify(method, path, query, key, doc)
        with self.lock:
            self.seen.append((test, step))
        if test not in ("T01", "unknown") and key not in KEYS:
            return (401, js({"error": "unauthorized"}), {"WWW-Authenticate": 'Bearer scope="mcp"'})
        if self.pick(test, step):
            return self.fault(test, step, path)
        return self.healthy(test, step, method, key, doc)

    def fault(self, test, step, path):
        form = self.form
        if form == "empty":
            return None
        if form == "hang":
            return "hang"
        if form == "500":
            return house_error(500, "Internal server error")
        if form == "401":
            return (401, js({"error": "unauthorized"}), {"WWW-Authenticate": 'Bearer scope="mcp"'})
        if form == "success:false":
            if path == "/api/synthesize/daily":
                return (200, js({"ok": False, "error": "synthesis failed"}))
            if path == "/mcp":
                return (200, js({"jsonrpc": "2.0", "id": 1 if test == "T17" else 2,
                                 "result": {"content": [{"type": "text", "text": "error: tool failed"}], "isError": True}}))
            return house_error(200, "the call failed")
        if form == "foreign":
            code = REFUSAL_STEPS[(test, step)]
            return (code, PROXY_PAGE.replace(b"{code}", str(code).encode()), {"Content-Type": "text/html"})
        if form == "noblocks":
            return (200, js({"success": True, "action": "guard-list", "count": 0}))
        raise ValueError(form)

    def healthy(self, test, step, method, key, doc):
        data = doc.get("data") or {}
        if test == "T01":
            return (401, js({"error": "unauthorized"}), {"WWW-Authenticate": 'Bearer scope="mcp"'})
        if test in ("gate", "T02"):
            return (200, js({"success": True, "action": "stats", "stats": {"total_blocks": self.total_blocks}}))
        if test == "T03":
            return (200, js({"success": True, "action": "stats", "stats": {"total_blocks": 3}}))
        if test == "T04":
            return (200, js({"success": True, "action": "guard-list", "count": 0, "blocks": []}))
        if test == "T09":
            return (200, js({"success": True, "action": "guard-list", "count": 2, "blocks": [
                {"id": "g1", "scope": "private", "guard_status": "clean"},
                {"id": "g2", "scope": "shared", "guard_status": "clean"}]}))
        if (test, step) in (("T05", 1), ("T06", 1), ("T22", 1)):
            bid = f"block-{len(self.titles) + 1}"
            with self.lock:
                self.titles[bid] = doc.get("title", "")
            block = {"id": bid, "title": doc.get("title")}
            if doc.get("sensitivity"):
                block.update(sensitivity=doc["sensitivity"], sensitivity_source="manual")
            return (200, js({"success": True, "action": "created", "block": block}))
        if (test, step) == ("T06", 2):
            bid = next((b for b, t in self.titles.items() if t == doc.get("title")), "")
            return (200, js({"success": True, "action": "noop", "existing_id": bid}))
        if (test, step) in (("T05", 2), ("T06", 3)):
            hits = [{"id": b, "title": t} for b, t in self.titles.items() if t == doc.get("query")]
            return (200, js({"success": True, "count": len(hits), "results": hits}))
        if (test, step) == ("T05", 3):
            return (200, js({"success": True, "block": {"id": doc.get("id"), "title": self.titles.get(doc.get("id"))}}))
        if test in ("cleanup", "unknown") or (test, step) == ("T05", 4):
            return (200, js({"success": True, "action": "delete", "deleted": {"id": doc.get("id")}}))
        if test == "T08":
            return (200, js({"success": True, "action": "guard-stats", "pending_count": 0}))
        if test == "T11":
            return (200, js({"success": True, "action": "dream-stats", "dream_checked": 5, "dream_links": 2}))
        if test == "T12":
            return (200, js({"success": True, "action": "dream-review", "items": []}))
        if (test, step) == ("T19", 1):
            return (200, js({"success": True, "stats": {"nodes": 3, "edges": 2, "truncated": False},
                             "nodes": [{"id": FOCUS, "title": "focus"}], "edges": []}))
        if (test, step) == ("T19", 2):
            return house_error(404, "Block not found")
        if (test, step) == ("T20", 1):
            return house_error(403, "admin key required")
        if (test, step) == ("T20", 2):
            return (200, js({"success": True, "setting": {"key": "rerank.blend_weight", "value": 0.3, "source": "default"}}))
        if (test, step) == ("T20", 3):
            return (200, js({"success": True, "key": "rerank.blend_weight", "value": 0.3, "source": "db"}))
        if (test, step) == ("T20", 4):
            return (200, js({"success": True, "key": "rerank.blend_weight", "value": 0.3, "source": "default"}))
        if (test, step) in (("T21", 1), ("T24", 1)):
            return house_error(403, "admin key required")
        if (test, step) == ("T21", 2):
            return (200, js({"success": True, "backends": [{"name": "a"}, {"name": "b"}, {"name": "c"}]}))
        if (test, step) == ("T22", 2):
            return house_error(400, "sensitivity downgrade personal → public opens this block to lower-trust backends"
                                    " — repeat with \"confirm_sensitivity_downgrade\": true")
        if (test, step) == ("T22", 3):
            return (200, js({"success": True, "block": {"id": doc.get("id"), "sensitivity": data.get("sensitivity"),
                                                         "metadata": {"sensitivity_audit": {"from": "personal", "to": "public"}}}}))
        if (test, step) == ("T24", 2):
            return (200, js({"success": True, "pending": 0, "by_source": {"manual": 1}}))
        if (test, step) == ("T24", 3):
            return (422, js({"success": False, "error": "validation failed", "fields": [
                {"field": "roles", "message": "classify role is hard-local — an external backend can never audit"
                                              " unclassified block content"}]}))
        if test == "T13":
            return (200, js({"success": True, "count": 3, "results": [{"id": "r1"}, {"id": "r2"}, {"id": "r3"}]}))
        if test == "T14":
            return (200, js({"success": True, "answer": "The Write Guard checks every write for near-duplicates.",
                             "confidence": "confident", "sources": [{"id": "s1"}]}))
        if test == "T15":
            return (200, js({"success": True, "answer": "I don't know based on the available sources.",
                             "confidence": "no_relevant_blocks_found", "sources": []}))
        if test == "T16":
            return (200, js({"success": True, "answer": "PostgreSQL keeps its data on the mount /var/lib/postgresql.",
                             "confidence": "confident", "sources": [{"id": "s2"}]}))
        if test == "T17":
            return (200, js({"jsonrpc": "2.0", "id": 1, "result": {"tools": [{"name": f"tool{i}"} for i in range(17)]}}))
        if test == "T18":
            return (200, js({"jsonrpc": "2.0", "id": 2, "result": {"content": [{"type": "text", "text": "recent: 1 block"}]}}))
        if test == "T23":
            return (200, js({"ok": True, "block_id": "digest-block-1"}))
        raise ValueError(f"no healthy answer for {test} step {step}")


DOCKER_SHIM = f"""#!/usr/bin/env bash
case "$1" in
  inspect) echo healthy; exit 0 ;;
  exec)
    sql="${{*: -1}}"
    case "$sql" in
      *context_dream_links*) echo {FOCUS} ;;
      *"scope='private'"*) echo {PRIV_BLOCK} ;;
      *context_llm_log*) echo 'stub-backend|internal' ;;
      *information_schema.tables*) echo 72 ;;
      *information_schema.columns*) echo 45 ;;
      *) echo "docker shim: no canned row for this query" >&2; exit 1 ;;
    esac
    exit 0 ;;
esac
echo "docker shim: refused $*" >&2
exit 1
"""
CTX_SHIM = """#!/usr/bin/env bash
if [[ "$1" == contract ]]; then echo "contract: ok (shim)"; exit 0; fi
echo "ctx shim: refused $*" >&2
exit 2
"""
UV_SHIM = """#!/usr/bin/env bash
echo "SKIP: uv shim of test-noanswer_test.sh (smoke.py t25 has its own self-test)"
exit 77
"""


def lowered(src):
    line = "MCP_TIMEOUT_S=30"
    if src.count("\n" + line + "\n") != 1:
        return src, False
    return src.replace("\n" + line + "\n", "\nMCP_TIMEOUT_S=1\n"), True


class Copy:
    """A directory with a copy of test.sh, scripts/lib, a throwaway .env
    pointing at a local port, a fresh backup dump (T10) and the shims."""

    def __init__(self, work, port):
        self.dir = tempfile.mkdtemp(dir=work)
        os.makedirs(os.path.join(self.dir, "scripts", "lib"))
        for lib in ("db-container.sh", "eval-api.sh"):
            src = os.path.join(REPO, "scripts", "lib", lib)
            if os.path.exists(src):
                shutil.copyfile(src, os.path.join(self.dir, "scripts", "lib", lib))
        with open(os.path.join(REPO, "test.sh")) as f:
            src, self.lowered = lowered(f.read())
        with open(os.path.join(self.dir, "test.sh"), "w") as f:
            f.write(src)
        os.makedirs(os.path.join(self.dir, "backups"))
        open(os.path.join(self.dir, "backups", "stub.dump"), "w").close()
        self.shim = os.path.join(self.dir, "shim")
        os.makedirs(self.shim)
        for name, text in (("docker", DOCKER_SHIM), ("ctx", CTX_SHIM), ("uv", UV_SHIM)):
            with open(os.path.join(self.shim, name), "w") as f:
                f.write(text)
            os.chmod(os.path.join(self.shim, name), 0o700)
        with open(os.path.join(self.dir, ".env"), "w") as f:
            f.write(f"WEBHOOK_BASE_URL=http://127.0.0.1:{port}\nCONTEXT_API_KEY_PRIVATE={PRIV}\n"
                    f"CONTEXT_API_KEY_ISOTEST={ISO}\nCTX_ADMIN_KEY={ADMIN}\n"
                    "CONTEXT_DB_PASSWORD=test-noanswer-not-a-password\n")
        self.argv_log = os.path.join(self.dir, "curl-argv.log")

    def unshimmed(self, path, spy=False):
        """'' when docker, ctx and uv (and curl, with spy) resolve to this
        copy's shims on path, else why not. On a TMPDIR mounted noexec the
        shell skips a shim and runs the real program next on PATH — the real
        ctx would talk to whatever server its default names."""
        want = {n: os.path.join(self.shim, n) for n in ("docker", "ctx", "uv")}
        if spy:
            want["curl"] = os.path.join(self.dir, "spy", "curl")
        for name, shim in want.items():
            got = shutil.which(name, path=path)
            if got != shim:
                return (f"{name} resolves to {got}, not to the shim {shim} — is TMPDIR "
                        f"({os.path.dirname(self.dir)}) mounted noexec? Set TMPDIR to a directory that allows exec.")
        return ""

    def run(self, args, spy=False):
        env = {k: v for k, v in os.environ.items()
               if not k.lower().endswith("_proxy")
               and not k.startswith(("CTX_", "CONTEXT_", "WEBHOOK_", "EVAL_", "OLLAMA_", "T25_", "PG", "XDG_"))}
        env["PATH"] = self.shim + ":" + env.get("PATH", "/usr/bin:/bin")
        if spy:
            # A curl first on PATH that writes its arguments to argv_log and runs
            # the real one: what any local user sees in the process list.
            d = os.path.join(self.dir, "spy")
            os.makedirs(d)
            with open(os.path.join(d, "curl"), "w") as f:
                f.write(f'#!/usr/bin/env bash\nprintf "%s\\n" "$@" >> "{self.argv_log}"\nexec "{REAL_CURL}" "$@"\n')
            os.chmod(os.path.join(d, "curl"), 0o700)
            env["PATH"] = d + ":" + env["PATH"]
        env["HOME"] = self.dir
        env["CURL_HOME"] = self.dir
        env["CTX_DB_CONTAINER"] = "test-noanswer-no-such-container"
        wrong = self.unshimmed(env["PATH"], spy)
        if wrong:
            raise RuntimeError(wrong)
        return subprocess.run(["bash", os.path.join(self.dir, "test.sh")] + args, capture_output=True,
                              text=True, env=env, timeout=240, cwd=self.dir)


LINE = re.compile(r"^\[(OK|FAIL|SKIP)\]\s+(T\d\d)\b(.*)$")


class Result:
    def __init__(self, p, stub, copy):
        self.rc = p.returncode
        self.out = p.stdout + p.stderr
        self.stdout = p.stdout
        self.verdicts, self.lines = {}, {}
        for line in p.stdout.splitlines():
            m = LINE.match(line)
            if m:
                self.verdicts[m.group(2)] = m.group(1)
                self.lines[m.group(2)] = line
        self.seen = list(stub.seen)
        self.lowered = copy.lowered
        self.argv = open(copy.argv_log).read() if os.path.exists(copy.argv_log) else ""

    def say(self):
        tail = self.out.strip().splitlines()[-4:]
        return f"rc={self.rc} verdicts={self.verdicts} tail={tail!r}"


def run_case(work, args, pick=None, form=None, total_blocks=500, spy=False):
    stub = Stub(pick, form, total_blocks)
    try:
        copy = Copy(work, stub.port)
        p = copy.run(args, spy=spy)
    finally:
        stub.close()
    return Result(p, stub, copy)


MODES = {
    "gate": lambda t, s: t == "gate",
    "first": lambda t, s: s == 1 and t not in ("gate", "cleanup", "unknown"),
    "step2": lambda t, s: s == 2 and t not in ("cleanup", "unknown"),
    "step3": lambda t, s: s == 3 and t not in ("cleanup", "unknown"),
    "step4": lambda t, s: s == 4 and t not in ("cleanup", "unknown"),
    "refusal": lambda t, s: (t, s) in REFUSAL_STEPS,
    "guardlist": lambda t, s: (t, s) in (("T04", 1), ("T09", 1)),
    "mcp": lambda t, s: t in ("T17", "T18"),
}


def table(name, res):
    print(f"    {name}: rc={res.rc} " + " ".join(f"{t}={res.verdicts.get(t, '-')}" for t in ALL_OLLAMA))


def main(work):
    probe = Copy(work, 9)
    wrong = probe.unshimmed(probe.shim + ":" + os.environ.get("PATH", "/usr/bin:/bin"))
    check(not wrong, "the shims run instead of docker, ctx and uv", wrong)
    if wrong:
        return
    plan = [("healthy", ["--with-ollama"], None, None, 500, True),
            ("healthy-system", [], None, None, 500, False),
            ("small-corpus", ["--with-ollama"], None, None, 50, False)]
    for mode in ("gate", "first", "step2", "step3", "step4", "refusal"):
        for form in FORMS:
            plan.append((f"{mode}/{form}", ["--with-ollama"], MODES[mode], form, 500, False))
    plan.append(("refusal/foreign", ["--with-ollama"], MODES["refusal"], "foreign", 500, False))
    plan.append(("guardlist/noblocks", ["--with-ollama"], MODES["guardlist"], "noblocks", 500, False))
    plan.append(("mcp/hang", ["--with-ollama"], MODES["mcp"], "hang", 500, False))
    with ThreadPoolExecutor(max_workers=4) as pool:
        futs = {name: pool.submit(run_case, work, args, pick, form, tb, spy) for name, args, pick, form, tb, spy in plan}
        res = {name: f.result() for name, f in futs.items()}

    print("--- verdict per test and run (- = no line)")
    for name, *_ in plan:
        table(name, res[name])

    print("--- 1. healthy stub")
    r = res["healthy"]
    want = {t: "OK" for t in ALL_OLLAMA}
    check(r.rc == 0 and {t: r.verdicts.get(t) for t in ALL_OLLAMA} == want and r.verdicts.get("T25") == "SKIP",
          "--with-ollama: rc 0, every test OK, T25 SKIP (uv shim)", r.say())
    leaked = [k for k in KEYS + ("deadbeef_invalid",) if k in r.argv]
    check(r.argv.count("--max-time") >= len(r.seen) and not leaked,
          "no key on a curl command line, a --max-time on every request",
          f"requests={len(r.seen)} max_time={r.argv.count('--max-time')} keys_on_command_line={len(leaked)}")
    r = res["healthy-system"]
    check(r.rc == 0 and {t: r.verdicts.get(t) for t in ALL_SYSTEM} == {t: "OK" for t in ALL_SYSTEM}
          and "Part 2: Retrieval Tests SKIPPED" in r.stdout,
          "without --with-ollama: rc 0, every system test OK, Part 2 skipped", r.say())

    print("--- 2. fewer than 100 blocks")
    r = res["small-corpus"]
    check(r.rc == 0 and all(r.verdicts.get(t) == "SKIP" and "corpus-bound" in r.lines.get(t, "") for t in CORPUS_BOUND)
          and all(r.verdicts.get(t) == "OK" for t in ALL_OLLAMA if t not in CORPUS_BOUND),
          "the corpus-bound tests SKIP with their reason, the rest OK, rc 0", r.say())

    print("--- 3. the corpus gate without an answer")
    for form in FORMS:
        r = res[f"gate/{form}"]
        named = "CONTEXT_API_KEY_PRIVATE" in r.out if form == "401" else "no answer" in r.out
        check(r.rc == 1 and not r.verdicts and "corpus gate" in r.out and named and PRIV not in r.out,
              f"{form}: rc 1, no test ran, the reason named", r.say())

    print("--- 4. the first request of every test without an answer")
    for form in FORMS:
        r = res[f"first/{form}"]
        want = {t: ("OK" if (t, form) == ("T01", "401") else "FAIL") for t in HTTP_TESTS}
        got = {t: r.verdicts.get(t) for t in HTTP_TESTS}
        unnamed = [t for t in HTTP_TESTS if want[t] == "FAIL" and t not in REFUSAL_TESTS
                   and "no answer" not in r.lines.get(t, "")]
        check(r.rc == 1 and got == want, f"{form}: every test FAILs (T01 passes on 401 alone), rc 1",
              f"rc={r.rc} wrong={ {t: got[t] for t in HTTP_TESTS if got[t] != want[t]} }")
        check(not unnamed and r.verdicts.get("T07") == "OK" and r.verdicts.get("T10") == "OK",
              f"{form}: each FAIL names the missing answer; T07 and T10 (no request) unaffected",
              f"unnamed={[r.lines.get(t) for t in unnamed][:3]}")

    print("--- 5. the follow-up requests without an answer, one step at a time")
    for step, tests in LATER_STEPS.items():
        for form in FORMS:
            r = res[f"step{step}/{form}"]
            got = {t: r.verdicts.get(t) for t in tests}
            check(r.rc == 1 and got == {t: "FAIL" for t in tests},
                  f"step {step}, {form}: {', '.join(tests)} FAIL, rc 1", f"rc={r.rc} got={got}")
    r = res["guardlist/noblocks"]
    got = {t: r.verdicts.get(t) for t in ("T04", "T09")}
    check(r.rc == 1 and got == {"T04": "FAIL", "T09": "FAIL"},
          "guard-list answering success:true without a blocks list: T04 and T09 FAIL", f"rc={r.rc} got={got}")

    print("--- 6. the refusal steps")
    for form in FORMS + ("foreign",):
        r = res[f"refusal/{form}"]
        want = {t: ("OK" if (t, form) == ("T01", "401") else "FAIL") for t in REFUSAL_TESTS}
        got = {t: r.verdicts.get(t) for t in REFUSAL_TESTS}
        check(r.rc == 1 and got == want,
              f"{form}: {', '.join(REFUSAL_TESTS)} FAIL{' (T01 passes on its 401)' if form == '401' else ''}, rc 1",
              f"rc={r.rc} wrong={ {t: got[t] for t in REFUSAL_TESTS if got[t] != want[t]} }")

    print("--- 7. every request with a --max-time; /mcp hanging")
    with open(os.path.join(REPO, "test.sh")) as f:
        raw = [ln for ln in f.read().splitlines() if re.search(r"\bcurl\b", ln.split("#", 1)[0])]
    check(not raw, "test.sh runs no curl of its own (every request goes through eval_api_request)",
          f"{len(raw)} lines, first: {raw[:2]!r}")
    r = res["mcp/hang"]
    check(r.lowered and r.rc == 1 and all(r.verdicts.get(t) == "FAIL" and "timeout" in r.lines.get(t, "")
                                          for t in ("T17", "T18")),
          "MCP_TIMEOUT_S bounds /mcp: T17 and T18 FAIL as a timeout",
          f"lowered={r.lowered} " + r.say())


work = tempfile.mkdtemp(prefix="test-noanswer-test.")
try:
    main(work)
finally:
    shutil.rmtree(work, ignore_errors=True)
print("")
print(f"  {passed} passed, {failed} failed")
sys.exit(1 if failed else 0)
PY
