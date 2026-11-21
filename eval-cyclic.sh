#!/usr/bin/env bash
#
# Context Store — Cyclic Temporal Retrieval Eval
# Usage: bash eval-cyclic.sh                       — run gegen HEAD, write .eval-cyclic-results-<branch>.json
#        bash eval-cyclic.sh --baseline            — wie oben, schreibt zusätzlich .eval-cyclic-baseline.json
#        bash eval-cyclic.sh --gold <path>         — alternatives Gold-File
#        bash eval-cyclic.sh --reference-date YYYY-MM-DD — frozen reference date
#        bash eval-cyclic.sh --branch <name>       — overrides git-branch im Output
#        bash eval-cyclic.sh --internal            — bypass reverse-proxy (uses http://ctx:8080 from n8nintern network; override via CTX_INTERNAL_URL)
#        CTX_EVAL_INSECURE=1 bash eval-cyclic.sh   — accept a self-signed TLS certificate (verification is on by default;
#                                                    a certificate that does not verify stops the run)
#        bash eval-cyclic.sh --help                — this text (the comment block at the top of the file)
#
# The key goes only to the base URL (and to the proxy http_proxy names, for an http:// one): a redirect is not
# followed (urllib would copy X-Context-Key to any Location) and stops the run naming its target — set the base URL
# to the address the server answers on.
#
# A case without an answer — no connection, a timeout, an HTTP error status, a body that is not a JSON object with
# success:true — is counted in n_errors and keeps its error. Exit status:
#   0  every case got an answer; the results (and with --baseline the baseline) are written.
#   1  some case got none: the results are written, the baseline never; or the run stopped without writing
#      anything — no case got an answer (that is no measurement), HTTP 401 (CONTEXT_API_KEY_PRIVATE is not a key
#      of that server), a redirect, or a TLS certificate that does not verify.
#   2  an unknown argument.
#
# Misst Top-K-Overlap (k=5) gegen .project/eval-cyclic-gold.json (default, korpus-spezifisch im privaten Submodule).
# Bewertungs-Methodologie: .project/eval-cyclic-methodology.md
#
# Source: https://github.com/GottZ/ctx

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${SCRIPT_DIR}/.env"
GOLD_FILE="${SCRIPT_DIR}/.project/eval-cyclic-gold.json"
BASELINE_FILE="${SCRIPT_DIR}/.eval-cyclic-baseline.json"
TIMESTAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
REFERENCE_DATE=""
WRITE_BASELINE=false
BRANCH_OVERRIDE=""
INTERNAL=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --baseline) WRITE_BASELINE=true; shift ;;
    --gold) GOLD_FILE="$2"; shift 2 ;;
    --reference-date) REFERENCE_DATE="$2"; shift 2 ;;
    --branch) BRANCH_OVERRIDE="$2"; shift 2 ;;
    --internal) INTERNAL=true; shift ;;
    # The help is the comment block at the top, up to its first line that is not a comment.
    -h|--help) awk 'NR == 1 { next } !/^#/ { exit } { print }' "$0"; exit 0 ;;
    *) echo "[FATAL] unknown arg: $1" >&2; exit 2 ;;
  esac
done

if [[ ! -f "$ENV_FILE" ]]; then
  echo "[FATAL] .env not found at $ENV_FILE" >&2
  exit 1
fi
set -a; source "$ENV_FILE"; set +a

if [[ ! -f "$GOLD_FILE" ]]; then
  echo "[FATAL] gold file not found: $GOLD_FILE" >&2
  exit 1
fi

# Webhook selection: --internal bypasses the reverse-proxy that drops 22-67% of
# LLM responses (Welle 35). Uses the ctx container DNS-name resolvable from the
# n8nintern compose network. Override with CTX_INTERNAL_URL if needed. Caller
# must run from inside the n8nintern network (e.g. `docker run --rm --network
# n8nintern …` or from another container attached to n8nintern). Container-IPs
# are unstable across restarts (W47-NEU-F), DNS-name is stable.
if $INTERNAL; then
  WEBHOOK="${CTX_INTERNAL_URL:-http://ctx:8080}"
  WEBHOOK_VAR=CTX_INTERNAL_URL
else
  WEBHOOK="${WEBHOOK_BASE_URL:?WEBHOOK_BASE_URL not in .env}"
  WEBHOOK_VAR=WEBHOOK_BASE_URL
fi
KEY_PRIVATE="${CONTEXT_API_KEY_PRIVATE:?CONTEXT_API_KEY_PRIVATE not in .env}"
GIT_REF="$(git -C "$SCRIPT_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)"
BRANCH="${BRANCH_OVERRIDE:-$(git -C "$SCRIPT_DIR" rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)}"
RESULTS_FILE="${SCRIPT_DIR}/.eval-cyclic-results-${BRANCH}.json"

run_eval() {
  python3 - "$GOLD_FILE" "$REFERENCE_DATE" "$GIT_REF" "$BRANCH" "$TIMESTAMP" "$WEBHOOK" "$KEY_PRIVATE" "$WEBHOOK_VAR" <<'PY'
import json, sys, urllib.request, urllib.error, urllib.parse, ssl, statistics, os

gold_path, ref_date, git_ref, branch, timestamp, webhook, api_key, webhook_var = sys.argv[1:9]

# Seconds a request may wait for the connection and for each read of the
# answer (the server streams a keepalive every 25 s while it synthesises).
TIMEOUT_S = 120

# OpenSSL X509_V_ERR_* codes (x509_vfy.h) of a certificate that does not
# verify, grouped by the advice that fits them (#460).
UNKNOWN_ISSUER = {2, 18, 19, 20, 21}  # issuer not trusted here: self-signed leaf or chain, unknown CA
NAME_MISMATCH = {62, 64}              # the certificate names another host or IP address
OUT_OF_VALIDITY = {9, 10}             # not yet valid, expired

with open(gold_path) as f:
    gold = json.load(f)

# CTX_EVAL_INSECURE=1 accepts the self-signed certificate of a local reverse
# proxy; verification is on otherwise (#445).
ctx = ssl.create_default_context()
if os.environ.get("CTX_EVAL_INSECURE") == "1":
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE

class RefusedRedirect(Exception):
    pass

def written(url):
    """scheme://host[:port] of url, without userinfo, path or query."""
    parts = urllib.parse.urlsplit(url)
    return f"{parts.scheme}://{parts.netloc.rpartition('@')[2]}"

class NoRedirect(urllib.request.HTTPRedirectHandler):
    """Follow no redirect (#445): urllib's own handler copies every request
    header to any Location, X-Context-Key with it. ctx answers /api/query
    itself, so a redirect means the base URL is not the server's address."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        fp.close()
        raise RefusedRedirect(
            f"refused a redirect (HTTP {code}) to {written(newurl)}: eval-cyclic.sh sends its key only to "
            f"{webhook_var}, {written(webhook)}, and follows no redirect; "
            f"set {webhook_var} to the address the server answers on")

opener = urllib.request.build_opener(NoRedirect, urllib.request.HTTPSHandler(context=ctx))

def tls_advice(err):
    """The advice for a certificate that does not verify, by its reason."""
    if err.verify_code in UNKNOWN_ISSUER:
        return "; CTX_EVAL_INSECURE=1 accepts a self-signed one, SSL_CERT_FILE can name the CA that signed it"
    if err.verify_code in NAME_MISMATCH:
        return f"; set {webhook_var} to a host name the certificate carries"
    if err.verify_code in OUT_OF_VALIDITY:
        return "; renew the certificate (or check this host's clock)"
    return ""

def query(q):
    """The server's answer to q, or {"error": …} when the case got none: no
    connection, a timeout, an HTTP error status, a body that is not a JSON
    object with success:true. A failure every case would share — a
    redirect, a certificate that does not verify, 401 — stops the run before
    anything is written: --baseline would store it."""
    body = json.dumps({"query": q}).encode("utf-8")
    req = urllib.request.Request(
        f"{webhook}/api/query",
        data=body,
        headers={"Content-Type":"application/json", "X-Context-Key": api_key},
        method="POST",
    )
    try:
        with opener.open(req, timeout=TIMEOUT_S) as resp:
            raw = resp.read()
    except RefusedRedirect as e:
        sys.exit(f"[FATAL] {e}")
    except urllib.error.HTTPError as e:
        e.close()
        if e.code == 401:
            sys.exit(f"[FATAL] {written(webhook)} ({webhook_var}) answered HTTP 401: CONTEXT_API_KEY_PRIVATE "
                     "is not a key of that server; nothing was written")
        return {"error": f"HTTP {e.code} {e.reason}"}
    except urllib.error.URLError as e:
        if isinstance(e.reason, ssl.SSLCertVerificationError):
            sys.exit(f"[FATAL] the TLS certificate of {written(webhook)} does not verify "
                     f"({e.reason.verify_message}){tls_advice(e.reason)}")
        return {"error": f"no answer: {e.reason}"}
    except Exception as e:
        # A timeout while reading, a connection the server closed, a broken
        # answer. The type alone: the text of some (an invalid header value,
        # e.g. a key with a CR from a CRLF .env) quotes the key.
        return {"error": f"no answer: {type(e).__name__}"}
    try:
        doc = json.loads(raw)
    except ValueError:
        return {"error": "the answer is not JSON"}
    if not isinstance(doc, dict):
        return {"error": "the answer is not a JSON object"}
    # Every answer of ctx's /api/query carries success:true; an object without
    # it comes from something else at that address. A failed answer is its
    # error and nothing else: its sources, confidence or dimension weights are
    # no measurement, and need not have the shape the loop reads.
    if doc.get("success") is not True:
        return {"error": doc.get("error") or doc.get("error_code")
                or f"success is {doc.get('success')!r}, not true"}
    return doc

cases = gold.get("cases", gold) if isinstance(gold, dict) else gold
if not cases:
    sys.exit(f"[FATAL] {gold_path} holds no cases")
buckets = {"C":[], "M":[], "L":[], "N":[], "F":[]}
case_results = []

for case in cases:
    cid = case["id"]
    bucket = case.get("bucket", "?")
    q = case["query"]
    expected = case.get("expected_top_k_block_ids", [])
    must_not = case.get("must_not_contain_block_ids", [])
    or_semantic = case.get("or_semantic", False)
    expected_dw = case.get("expected_dim_weights") or {}

    resp = query(q)
    # /api/query returns "sources" (LLM-synthesis-filtered subset of RRF top-K).
    # /api/search returns "results" (raw FTS+trigram). We use sources because that
    # matches end-to-end user-facing retrieval — see methodology doc.
    results = resp.get("sources", resp.get("results", [])) or []
    top5 = [r.get("id") for r in results[:5]]

    if expected:
        if or_semantic:
            top_k_pass = any(eid in top5 for eid in expected)
            top_k_overlap = 1.0 if top_k_pass else 0.0
        else:
            hits = sum(1 for eid in expected if eid in top5)
            top_k_pass = hits == len(expected)
            top_k_overlap = hits / len(expected)
    else:
        top_k_pass = True   # N-bucket: pass if no expectation set
        top_k_overlap = None

    must_not_pass = not any(mid in top5 for mid in must_not) if must_not else True

    # dim_weight_pass (methodology): expected_dim_weights ⊆ activated_dim_weights —
    # ungewichtete Vergleichsmenge, d.h. Key-Ebene: jede erwartete Dimension muss
    # mit Gewicht > 0 aktiviert sein. activated_dim_weights liefert /api/query
    # seit A-W1. Cases ohne expected_dim_weights: vacuous pass (Schiene inaktiv);
    # heute trägt nur Bucket F das Feld, C/M/L-Semantik bleibt unverändert.
    activated_dw = resp.get("activated_dim_weights") or {}
    if expected_dw:
        dim_weight_pass = all(activated_dw.get(k, 0) > 0 for k in expected_dw)
    else:
        dim_weight_pass = True

    case_pass = top_k_pass and must_not_pass and dim_weight_pass

    case_results.append({
        "id": cid,
        "bucket": bucket,
        "query": q,
        "top5": top5,
        "expected": expected,
        "top_k_pass": top_k_pass,
        "must_not_pass": must_not_pass,
        "expected_dim_weights": expected_dw or None,
        "activated_dim_weights": activated_dw or None,
        "dim_weight_pass": dim_weight_pass,
        "case_pass": case_pass,
        "top_k_overlap": top_k_overlap,
        "confidence": resp.get("confidence"),
        "translated": resp.get("translated"),
        "error": resp.get("error"),
    })
    if bucket in buckets:
        buckets[bucket].append(case_results[-1])

# A run in which no case got an answer measured nothing: its zeros would read
# as a retrieval result and --baseline would store them (#460).
n_errors = sum(1 for c in case_results if c["error"] is not None)
if n_errors == len(case_results):
    sys.exit(f"[FATAL] no case got an answer from {written(webhook)} ({webhook_var}): {n_errors} of {n_errors} failed, "
             f"the first with: {case_results[0]['error']}; that is no measurement — nothing was written")

per_bucket = {}
for b, items in buckets.items():
    if not items:
        per_bucket[b] = {"n": 0, "passes": 0, "pass_rate": None, "top_k_overlap_at_5": None}
        continue
    n = len(items)
    passes = sum(1 for c in items if c["case_pass"])
    overlaps = [c["top_k_overlap"] for c in items if c["top_k_overlap"] is not None]
    per_bucket[b] = {
        "n": n,
        "passes": passes,
        "pass_rate": round(passes/n, 4),
        "top_k_overlap_at_5": round(statistics.mean(overlaps), 4) if overlaps else None,
    }

valid_rates = [v["pass_rate"] for v in per_bucket.values() if v["pass_rate"] is not None]
mean_pass_rate = round(statistics.mean(valid_rates), 4) if valid_rates else 0.0

# dim_weight-Schiene aggregiert über alle Cases, die sie messen (Branch W).
dw_cases = [c for c in case_results if c["expected_dim_weights"]]
dim_weight_pass_rate = (
    round(sum(1 for c in dw_cases if c["dim_weight_pass"]) / len(dw_cases), 4)
    if dw_cases else None
)

out = {
    "ref": git_ref,
    "branch": branch,
    "timestamp": timestamp,
    "reference_date": ref_date or None,
    "reference_date_frozen": bool(ref_date),
    "n_cases": len(case_results),
    "n_errors": n_errors,
    "per_bucket": per_bucket,
    "mean_pass_rate": mean_pass_rate,
    "dim_weight_pass_rate": dim_weight_pass_rate,
    "case_results": case_results,
}
print(json.dumps(out, indent=2, ensure_ascii=False))
PY
}

echo "[eval-cyclic] gold=$GOLD_FILE branch=$BRANCH ref=$GIT_REF"
RESULT_JSON=$(run_eval)
RUN_RC=$?
# A stopped run said why on stderr already, and writes nothing.
if (( RUN_RC != 0 )); then
  exit 1
fi
if [[ -z "$RESULT_JSON" ]]; then
  echo "[FATAL] empty result" >&2
  exit 1
fi
echo "$RESULT_JSON" > "$RESULTS_FILE"
echo "[eval-cyclic] wrote $RESULTS_FILE"

N_ERRORS=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["n_errors"])' "$RESULTS_FILE")
if $WRITE_BASELINE; then
  if [[ "$N_ERRORS" == 0 ]]; then
    echo "$RESULT_JSON" > "$BASELINE_FILE"
    echo "[eval-cyclic] wrote $BASELINE_FILE"
  else
    echo "[eval-cyclic] $N_ERRORS case(s) got no answer: the baseline was not written" >&2
  fi
fi

# Summary line
python3 - "$RESULTS_FILE" <<'PY'
import json, sys
with open(sys.argv[1]) as f:
    d = json.load(f)
pb = d["per_bucket"]
dwr = d.get("dim_weight_pass_rate")
print(f"branch={d['branch']} ref={d['ref']} cases={d['n_cases']} errors={d['n_errors']} mean_pass={d['mean_pass_rate']} dim_weight_pass={dwr}")
for b in ("C","M","L","N","F"):
    v = pb.get(b, {})
    print(f"  {b}: n={v.get('n',0)} passes={v.get('passes',0)} pass_rate={v.get('pass_rate')} overlap@5={v.get('top_k_overlap_at_5')}")
for c in d["case_results"]:
    if c.get("error") is not None:
        print(f"  no answer: {c['id']}: {c['error']}")
PY

if [[ "$N_ERRORS" != 0 ]]; then
  exit 1
fi
