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
#
# The key goes only to the base URL (and to the proxy http_proxy names, for an http:// one): a redirect is not
# followed (urllib would copy X-Context-Key to any Location) and stops the run naming its target — set the base URL
# to the address the server answers on.
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
    -h|--help) sed -n '1,22p' "$0"; exit 0 ;;
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

def query(q):
    body = json.dumps({"query": q}).encode("utf-8")
    req = urllib.request.Request(
        f"{webhook}/api/query",
        data=body,
        headers={"Content-Type":"application/json", "X-Context-Key": api_key},
        method="POST",
    )
    try:
        with opener.open(req, timeout=120) as resp:
            return json.loads(resp.read())
    except RefusedRedirect as e:
        sys.exit(f"[FATAL] {e}")
    except urllib.error.URLError as e:
        if isinstance(e.reason, ssl.SSLCertVerificationError):
            # Every case would fail the same way, and --baseline would store it.
            sys.exit(f"[FATAL] the TLS certificate of {written(webhook)} does not verify ({e.reason.verify_message}); "
                     "CTX_EVAL_INSECURE=1 accepts a self-signed one")
        return {"error": str(e), "results": []}
    except Exception as e:
        return {"error": str(e), "results": []}

cases = gold.get("cases", gold) if isinstance(gold, dict) else gold
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
if [[ -z "$RESULT_JSON" ]]; then
  echo "[FATAL] empty result" >&2
  exit 1
fi
echo "$RESULT_JSON" > "$RESULTS_FILE"
echo "[eval-cyclic] wrote $RESULTS_FILE"

if $WRITE_BASELINE; then
  echo "$RESULT_JSON" > "$BASELINE_FILE"
  echo "[eval-cyclic] wrote $BASELINE_FILE"
fi

# Summary line
python3 - "$RESULTS_FILE" <<'PY'
import json, sys
with open(sys.argv[1]) as f:
    d = json.load(f)
pb = d["per_bucket"]
dwr = d.get("dim_weight_pass_rate")
print(f"branch={d['branch']} ref={d['ref']} cases={d['n_cases']} mean_pass={d['mean_pass_rate']} dim_weight_pass={dwr}")
for b in ("C","M","L","N","F"):
    v = pb.get(b, {})
    print(f"  {b}: n={v.get('n',0)} passes={v.get('passes',0)} pass_rate={v.get('pass_rate')} overlap@5={v.get('top_k_overlap_at_5')}")
PY
