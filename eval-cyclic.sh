#!/usr/bin/env bash
#
# Context Store — Cyclic Temporal Retrieval Eval
# Usage: bash eval-cyclic.sh                       — run gegen HEAD, write .eval-cyclic-results-<branch>.json
#        bash eval-cyclic.sh --baseline            — wie oben, schreibt zusätzlich .eval-cyclic-baseline.json
#        bash eval-cyclic.sh --gold <path>         — alternatives Gold-File
#        bash eval-cyclic.sh --reference-date YYYY-MM-DD — frozen reference date
#        bash eval-cyclic.sh --branch <name>       — overrides git-branch im Output
#        bash eval-cyclic.sh --internal            — bypass reverse-proxy (use ctx container IP)
#
# Misst Top-K-Overlap (k=5) gegen .eval-cyclic-gold.json.
# Bewertungs-Methodologie: .project/eval-cyclic-methodology.md
#
# Source: https://github.com/GottZ/ctx

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${SCRIPT_DIR}/.env"
GOLD_FILE="${SCRIPT_DIR}/.eval-cyclic-gold.json"
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
    -h|--help) sed -n '1,16p' "$0"; exit 0 ;;
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
# LLM responses (Welle 35). 172.32.16.4 is the ctx container IP — valid in the
# n8nintern compose network. Verify via:
#   docker inspect ctx --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}'
if $INTERNAL; then
  WEBHOOK="http://172.32.16.4:8080"
else
  WEBHOOK="${WEBHOOK_BASE_URL:?WEBHOOK_BASE_URL not in .env}"
fi
KEY_PRIVATE="${CONTEXT_API_KEY_PRIVATE:?CONTEXT_API_KEY_PRIVATE not in .env}"
GIT_REF="$(git -C "$SCRIPT_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)"
BRANCH="${BRANCH_OVERRIDE:-$(git -C "$SCRIPT_DIR" rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)}"
RESULTS_FILE="${SCRIPT_DIR}/.eval-cyclic-results-${BRANCH}.json"

api_query() {
  local query="$1"
  local timeout="${2:-60}"
  local payload
  payload=$(python3 -c "import sys,json; print(json.dumps({'query':sys.argv[1]}))" "$query")
  curl -sk --max-time "$timeout" -X POST "$WEBHOOK/api/query" \
    -H "Content-Type: application/json" \
    -H "X-Context-Key: $KEY_PRIVATE" \
    -d "$payload" 2>/dev/null
}

run_eval() {
  python3 - "$GOLD_FILE" "$REFERENCE_DATE" "$GIT_REF" "$BRANCH" "$TIMESTAMP" "$WEBHOOK" "$KEY_PRIVATE" <<'PY'
import json, sys, urllib.request, urllib.error, ssl, statistics, os

gold_path, ref_date, git_ref, branch, timestamp, webhook, api_key = sys.argv[1:8]

with open(gold_path) as f:
    gold = json.load(f)

ctx = ssl.create_default_context()
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE

def query(q):
    body = json.dumps({"query": q}).encode("utf-8")
    req = urllib.request.Request(
        f"{webhook}/api/query",
        data=body,
        headers={"Content-Type":"application/json", "X-Context-Key": api_key},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=120, context=ctx) as resp:
            return json.loads(resp.read())
    except urllib.error.URLError as e:
        return {"error": str(e), "results": []}
    except Exception as e:
        return {"error": str(e), "results": []}

cases = gold.get("cases", gold) if isinstance(gold, dict) else gold
buckets = {"C":[], "M":[], "L":[], "N":[]}
case_results = []

for case in cases:
    cid = case["id"]
    bucket = case.get("bucket", "?")
    q = case["query"]
    expected = case.get("expected_top_k_block_ids", [])
    must_not = case.get("must_not_contain_block_ids", [])
    or_semantic = case.get("or_semantic", False)

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
    case_pass = top_k_pass and must_not_pass

    case_results.append({
        "id": cid,
        "bucket": bucket,
        "query": q,
        "top5": top5,
        "expected": expected,
        "top_k_pass": top_k_pass,
        "must_not_pass": must_not_pass,
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

out = {
    "ref": git_ref,
    "branch": branch,
    "timestamp": timestamp,
    "reference_date": ref_date or None,
    "reference_date_frozen": bool(ref_date),
    "n_cases": len(case_results),
    "per_bucket": per_bucket,
    "mean_pass_rate": mean_pass_rate,
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
print(f"branch={d['branch']} ref={d['ref']} cases={d['n_cases']} mean_pass={d['mean_pass_rate']}")
for b in ("C","M","L","N"):
    v = pb.get(b, {})
    print(f"  {b}: n={v.get('n',0)} passes={v.get('passes',0)} pass_rate={v.get('pass_rate')} overlap@5={v.get('top_k_overlap_at_5')}")
PY
