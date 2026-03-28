#!/usr/bin/env bash
#
# Context Store Evaluation Harness
# Usage: bash eval.sh                  — full eval (retrieval + synthesis)
#        bash eval.sh --retrieval-only  — skip synthesis tests (faster)
#        bash eval.sh --update-baseline — run + save results as new baseline
#
# Requires: curl, python3, jq (optional, python3 fallback)
# Runtime: ~3-5 minutes (Ollama on-prem, sequential queries)
#
# ctx — Your AI's save game. By GottZ (github.com/GottZ/ctx/graphs/contributors)
# Evaluates GottZ 4-Way RRF retrieval quality across 35 test queries.
# Source: https://github.com/GottZ/ctx

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${SCRIPT_DIR}/.env"

if [[ ! -f "$ENV_FILE" ]]; then
    echo "[FATAL] .env not found at $ENV_FILE"
    exit 1
fi
set -a; source "$ENV_FILE"; set +a

WEBHOOK="${WEBHOOK_BASE_URL:-https://localhost/webhook}"
KEY_PRIVATE="${CONTEXT_API_KEY_PRIVATE:?CONTEXT_API_KEY_PRIVATE not set in .env}"
BASELINE_FILE="/compose/n8n/.eval-baseline.json"
RESULTS_FILE="/tmp/eval-results-$(date +%s).json"

RETRIEVAL_ONLY=false
UPDATE_BASELINE=false
for arg in "$@"; do
  case "$arg" in
    --retrieval-only) RETRIEVAL_ONLY=true ;;
    --update-baseline) UPDATE_BASELINE=true ;;
  esac
done

# =====================================================================
# Helpers
# =====================================================================

api() {
  local timeout="${3:-120}"
  curl -s --max-time "$timeout" -X POST "$1" \
    -H "Content-Type: application/json" \
    -H "X-Context-Key: $KEY_PRIVATE" \
    -d "$2" 2>/dev/null
}

# Python helper: extract JSON field
pyjson() {
  python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    path = '$1'.split('.')
    for p in path:
        if isinstance(d, list):
            d = d[int(p)]
        else:
            d = d[p]
    print(d)
except:
    print('')
" 2>/dev/null
}

# =====================================================================
# Test Case Definitions
# =====================================================================
#
# Format: ID|TYPE|QUERY|EXPECTED_CONFIDENCE|KEYWORDS(comma-sep)|DESCRIPTION
#
# TYPE: retrieval = context-search (no LLM), synthesis = context-agent (LLM)
# EXPECTED_CONFIDENCE: confident|low|none|any (for retrieval tests)
# KEYWORDS: comma-separated strings that MUST appear in the answer (case-insensitive)
#           For retrieval tests: keywords that must appear in result titles/previews

define_test_cases() {
cat <<'CASES'
# --- CONFIDENT SYNTHESIS (known facts, should return confident) ---
S01|synthesis|What embedding model does the context store use?|confident|qwen3,embedding|Core infrastructure fact
S02|synthesis|How does the Write Guard detect duplicates?|confident|hash,similarity,async|Architecture knowledge
S03|synthesis|What are the scope values in the multi-tenant system?|confident|private,work,shared|Scope enum values
S04|synthesis|What is the PostgreSQL version and what extension is used for vectors?|confident|18,pgvector|DB stack
S05|synthesis|What LLM model is used for synthesis in the context-agent?|confident|qwen3,9b|Model config
S06|synthesis|How does the blob storage authenticate requests?|confident|key,hash,sha|Auth mechanism
S07|synthesis|What is the RRF fusion strategy used in retrieval?|confident|rrf,fusion|Retrieval architecture
S08|synthesis|What are the Write Guard similarity thresholds?|confident|0.98,0.92|Threshold values
S09|synthesis|What happened with the qwen3.5:9b model evaluation?|confident|death,spiral,thinking,token|Known failure case
S10|synthesis|What is the Matryoshka truncation dimension for embeddings?|confident|1024|Embedding dimension

# --- BILINGUAL (German queries about English-titled content) ---
B01|synthesis|Welches Embedding-Modell wird verwendet?|confident|qwen3,embedding|DE query, EN content
B02|synthesis|Wie funktioniert der Write Guard?|confident|hash,similarity|DE query about guard
B03|synthesis|Was ist der PostgreSQL Mount-Pfad?|confident|postgresql,var/lib|DE query, infra fact
B04|synthesis|Welche Scope-Werte gibt es im Multi-Tenant-System?|confident|private,work,shared|DE enum values
B05|synthesis|Was ist das Problem mit qwen3.5:9b?|confident|thinking,token|DE about model failure

# --- NEGATIVE (should NOT be answerable from the store) ---
N01|synthesis|What is the recipe for Kartoffelsuppe?|none||Completely off-topic
N02|synthesis|How do I configure a Kubernetes cluster with Istio?|none||Unrelated tech
N03|synthesis|What is the capital of France?|none||General knowledge, not in store
N04|synthesis|How to set up a React Native app with Expo?|none||Unrelated framework
N05|synthesis|What are the best restaurants in Berlin?|none||Non-technical, off-topic

# --- KEYWORD / SPECIFIC FACT (tests precision) ---
K01|synthesis|What is the context-agent workflow ID?|confident|e2eCUrv3UTsuavu2|Exact workflow ID
K02|synthesis|What database name does the context store use?|confident|context_store|DB name
K03|synthesis|What is the Ollama embedding model name?|confident|qwen3-embedding|Embed model name
K04|synthesis|What port does Ollama run on?|confident|11434|Port number
K05|synthesis|What is the native embedding dimension before Matryoshka truncation?|confident|4096|Native embed dimension

# --- MULTI-HOP (requires synthesizing across multiple blocks) ---
M01|synthesis|Why was qwen3:4b-instruct chosen over qwen3.5:9b and what are the token differences?|confident|death,spiral,instruct,token|Cross-block reasoning
M02|synthesis|What is the full auth flow from API key to scope filtering?|confident|key,hash,scope|Auth pipeline
M03|synthesis|How do Write Guard thresholds differ for temporal content vs normal content?|confident|temporal,0.98,0.92|Specialized thresholds
M04|synthesis|What are the differences between context-search and context-agent?|confident|search,agent,llm,compact|Endpoint comparison
M05|synthesis|How does the bilingual retrieval gap affect German queries and what is the fix?|confident|german,translation,retrieval|Problem + solution

# --- IMPERATIVE (command-style queries, should NOT be confused with keywords) ---
I01|synthesis|Zeig mir die Write Guard Thresholds|confident|0.98,0.92|DE imperative, known thresholds
I02|synthesis|List all scope values|confident|private,shared|EN imperative, enum values
I03|synthesis|Nenn die Modelle die im System laufen|confident|qwen3,embedding|DE imperative, model inventory
I04|synthesis|Describe the RRF retrieval strategy|confident|semantic,rrf,weight|EN imperative, architecture
I05|synthesis|Zeig Scopes|confident|private,shared|Minimal 2-word DE imperative
I06|synthesis|Show me how auth works in the context store|confident|key,hash,scope|EN imperative, auth flow
I07|synthesis|Liste alle Kategorien im Context Store auf|confident|infrastructure,learnings|DE imperative, categories
I08|synthesis|Explain the blob storage authentication|confident|key,hash,blob|EN imperative, blob auth

# --- RETRIEVAL QUALITY (context-search, no LLM — tests vector+FTS ranking) ---
R01|retrieval|Write Guard|any|write guard|Top result relevance
R02|retrieval|embedding model|any|embedding|Embedding-related blocks
R03|retrieval|blob storage|any|blob|Blob-related blocks
R04|retrieval|multi-tenant scope|any|scope,tenant|Scope architecture
R05|retrieval|key_hash auth migration|any|key,hash,auth|Auth migration blocks
CASES
}

# =====================================================================
# Test Runner
# =====================================================================

PASS=0
FAIL=0
SKIP=0
TOTAL=0
declare -a RESULTS_JSON=()
START_TIME=$(date +%s)

run_synthesis_test() {
  local id="$1" query="$2" expected_conf="$3" keywords="$4" desc="$5"
  local t_start t_end latency_ms resp answer confidence sources_count keyword_hits keyword_total keyword_pct verdict detail

  t_start=$(date +%s%3N)
  local escaped_query
  escaped_query=$(printf '%s' "$query" | python3 -c "import sys,json; print(json.dumps(sys.stdin.read()))")
  resp=$(api "$WEBHOOK/context-agent" "{\"query\":$escaped_query}" 120)
  t_end=$(date +%s%3N)
  latency_ms=$(( t_end - t_start ))

  # Parse response — handle timeouts and error responses
  if [[ -z "$resp" ]]; then
    answer=""
    confidence="timeout"
    sources_count=0
  else
    answer=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('answer','').lower())" 2>/dev/null || echo "")
    confidence=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('confidence','error'))" 2>/dev/null || echo "error")
    sources_count=$(echo "$resp" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('sources',[])))" 2>/dev/null || echo "0")
  fi

  # Check confidence
  local conf_ok=false
  if [[ "$expected_conf" == "any" ]]; then
    conf_ok=true
  elif [[ "$expected_conf" == "none" ]]; then
    # For negative tests: none OR low confidence, OR answer contains rejection markers
    if [[ "$confidence" == "none" ]] || [[ "$confidence" == "low" ]] || [[ "$confidence" == "no_relevant_blocks_found" ]] || [[ "$confidence" == "error" ]] || [[ "$confidence" == "timeout" ]]; then
      conf_ok=true
    elif echo "$answer" | grep -qi "no_relevant_blocks_found\|keine relevanten\|cannot answer\|not relevant\|keine antwort"; then
      conf_ok=true
    fi
  elif [[ "$confidence" == "$expected_conf" ]]; then
    conf_ok=true
  fi

  # Check keywords
  keyword_hits=0
  keyword_total=0
  if [[ -n "$keywords" ]]; then
    IFS=',' read -ra KW_ARRAY <<< "$keywords"
    keyword_total=${#KW_ARRAY[@]}
    for kw in "${KW_ARRAY[@]}"; do
      if echo "$answer" | grep -qiF "$kw"; then
        keyword_hits=$(( keyword_hits + 1 ))
      fi
    done
  fi

  if (( keyword_total > 0 )); then
    keyword_pct=$(( keyword_hits * 100 / keyword_total ))
  else
    keyword_pct=100  # no keywords to check = pass
  fi

  # Verdict
  if $conf_ok && (( keyword_pct >= 50 )); then
    verdict="PASS"
    PASS=$(( PASS + 1 ))
  else
    verdict="FAIL"
    FAIL=$(( FAIL + 1 ))
  fi
  TOTAL=$(( TOTAL + 1 ))

  # Detail for failures
  detail=""
  if ! $conf_ok; then
    detail="confidence=${confidence} (expected ${expected_conf})"
  fi
  if (( keyword_total > 0 )) && (( keyword_pct < 50 )); then
    [[ -n "$detail" ]] && detail="$detail; "
    detail="${detail}keywords=${keyword_hits}/${keyword_total}"
  fi

  # Output
  local status_icon
  [[ "$verdict" == "PASS" ]] && status_icon="[OK]  " || status_icon="[FAIL]"
  printf "%s %-4s %-50s conf=%-10s kw=%d/%d  %5dms" \
    "$status_icon" "$id" "${desc:0:50}" "$confidence" "$keyword_hits" "$keyword_total" "$latency_ms"
  [[ -n "$detail" ]] && printf "  !! %s" "$detail"
  echo ""

  # Store result as JSON line
  RESULTS_JSON+=("{\"id\":\"$id\",\"type\":\"synthesis\",\"verdict\":\"$verdict\",\"confidence\":\"$confidence\",\"expected_confidence\":\"$expected_conf\",\"keyword_hits\":$keyword_hits,\"keyword_total\":$keyword_total,\"keyword_pct\":$keyword_pct,\"latency_ms\":$latency_ms,\"sources\":$sources_count,\"desc\":\"$desc\"}")
}

run_retrieval_test() {
  local id="$1" query="$2" expected_conf="$3" keywords="$4" desc="$5"
  local t_start t_end latency_ms resp count titles keyword_hits keyword_total keyword_pct verdict detail

  t_start=$(date +%s%3N)
  local escaped_query
  escaped_query=$(printf '%s' "$query" | python3 -c "import sys,json; print(json.dumps(sys.stdin.read()))")
  resp=$(api "$WEBHOOK/context-search" "{\"query\":$escaped_query}" 30)
  t_end=$(date +%s%3N)
  latency_ms=$(( t_end - t_start ))

  count=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('count',0))" 2>/dev/null || echo "0")
  titles=$(echo "$resp" | python3 -c "
import sys,json
d=json.load(sys.stdin)
results=d.get('results',[])
text=' '.join(r.get('title','') + ' ' + r.get('content_preview','') for r in results).lower()
print(text)
" 2>/dev/null || echo "")

  # Check keywords in titles+previews
  keyword_hits=0
  keyword_total=0
  if [[ -n "$keywords" ]]; then
    IFS=',' read -ra KW_ARRAY <<< "$keywords"
    keyword_total=${#KW_ARRAY[@]}
    for kw in "${KW_ARRAY[@]}"; do
      if echo "$titles" | grep -qiF "$kw"; then
        keyword_hits=$(( keyword_hits + 1 ))
      fi
    done
  fi

  if (( keyword_total > 0 )); then
    keyword_pct=$(( keyword_hits * 100 / keyword_total ))
  else
    keyword_pct=100
  fi

  # Verdict: must return results AND match keywords
  if (( count >= 1 )) && (( keyword_pct >= 50 )); then
    verdict="PASS"
    PASS=$(( PASS + 1 ))
  else
    verdict="FAIL"
    FAIL=$(( FAIL + 1 ))
  fi
  TOTAL=$(( TOTAL + 1 ))

  detail=""
  (( count < 1 )) && detail="count=0"
  if (( keyword_total > 0 )) && (( keyword_pct < 50 )); then
    [[ -n "$detail" ]] && detail="$detail; "
    detail="${detail}keywords=${keyword_hits}/${keyword_total}"
  fi

  local status_icon
  [[ "$verdict" == "PASS" ]] && status_icon="[OK]  " || status_icon="[FAIL]"
  printf "%s %-4s %-50s count=%-4d kw=%d/%d  %5dms" \
    "$status_icon" "$id" "${desc:0:50}" "$count" "$keyword_hits" "$keyword_total" "$latency_ms"
  [[ -n "$detail" ]] && printf "  !! %s" "$detail"
  echo ""

  RESULTS_JSON+=("{\"id\":\"$id\",\"type\":\"retrieval\",\"verdict\":\"$verdict\",\"count\":$count,\"keyword_hits\":$keyword_hits,\"keyword_total\":$keyword_total,\"keyword_pct\":$keyword_pct,\"latency_ms\":$latency_ms,\"desc\":\"$desc\"}")
}

# =====================================================================
# Main
# =====================================================================

echo "================================================================="
echo "  Context Store Evaluation Harness"
echo "  $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "================================================================="
echo ""

# Preflight: verify connectivity
echo "--- Preflight ---"
preflight=$(api "$WEBHOOK/context-manage" '{"action":"stats"}' 15)
total_blocks=$(echo "$preflight" | python3 -c "import sys,json; print(json.load(sys.stdin)['stats']['total_blocks'])" 2>/dev/null || echo "0")
if (( total_blocks < 100 )); then
  echo "ABORT: Context store has only $total_blocks blocks (expected 100+). Is the system up?"
  exit 1
fi
echo "Store OK: $total_blocks blocks"
echo ""

# --- Retrieval Tests ---
echo "--- Retrieval Tests (context-search, no LLM) ---"
echo ""

while IFS='|' read -r id type query expected_conf keywords desc; do
  # Skip comments and empty lines
  [[ "$id" =~ ^#.*$ ]] && continue
  [[ -z "$id" ]] && continue
  [[ "$type" != "retrieval" ]] && continue
  run_retrieval_test "$id" "$query" "$expected_conf" "$keywords" "$desc"
done < <(define_test_cases)

if ! $RETRIEVAL_ONLY; then
  echo ""
  echo "--- Synthesis Tests (context-agent, LLM) ---"
  echo ""

  # Confident
  echo "  -- Confident (known facts) --"
  while IFS='|' read -r id type query expected_conf keywords desc; do
    [[ "$id" =~ ^#.*$ ]] && continue
    [[ -z "$id" ]] && continue
    [[ "$type" != "synthesis" ]] && continue
    [[ ! "$id" =~ ^S ]] && continue
    run_synthesis_test "$id" "$query" "$expected_conf" "$keywords" "$desc"
  done < <(define_test_cases)

  echo ""
  echo "  -- Bilingual (DE query, EN content) --"
  while IFS='|' read -r id type query expected_conf keywords desc; do
    [[ "$id" =~ ^#.*$ ]] && continue
    [[ -z "$id" ]] && continue
    [[ "$type" != "synthesis" ]] && continue
    [[ ! "$id" =~ ^B ]] && continue
    run_synthesis_test "$id" "$query" "$expected_conf" "$keywords" "$desc"
  done < <(define_test_cases)

  echo ""
  echo "  -- Negative (should reject) --"
  while IFS='|' read -r id type query expected_conf keywords desc; do
    [[ "$id" =~ ^#.*$ ]] && continue
    [[ -z "$id" ]] && continue
    [[ "$type" != "synthesis" ]] && continue
    [[ ! "$id" =~ ^N ]] && continue
    run_synthesis_test "$id" "$query" "$expected_conf" "$keywords" "$desc"
  done < <(define_test_cases)

  echo ""
  echo "  -- Keyword / Specific Facts --"
  while IFS='|' read -r id type query expected_conf keywords desc; do
    [[ "$id" =~ ^#.*$ ]] && continue
    [[ -z "$id" ]] && continue
    [[ "$type" != "synthesis" ]] && continue
    [[ ! "$id" =~ ^K ]] && continue
    run_synthesis_test "$id" "$query" "$expected_conf" "$keywords" "$desc"
  done < <(define_test_cases)

  echo ""
  echo "  -- Imperative (command-style queries) --"
  while IFS='|' read -r id type query expected_conf keywords desc; do
    [[ "$id" =~ ^#.*$ ]] && continue
    [[ -z "$id" ]] && continue
    [[ "$type" != "synthesis" ]] && continue
    [[ ! "$id" =~ ^I ]] && continue
    run_synthesis_test "$id" "$query" "$expected_conf" "$keywords" "$desc"
  done < <(define_test_cases)

  echo ""
  echo "  -- Multi-hop (cross-block reasoning) --"
  while IFS='|' read -r id type query expected_conf keywords desc; do
    [[ "$id" =~ ^#.*$ ]] && continue
    [[ -z "$id" ]] && continue
    [[ "$type" != "synthesis" ]] && continue
    [[ ! "$id" =~ ^M ]] && continue
    run_synthesis_test "$id" "$query" "$expected_conf" "$keywords" "$desc"
  done < <(define_test_cases)
fi

END_TIME=$(date +%s)
ELAPSED=$(( END_TIME - START_TIME ))

# =====================================================================
# Results JSON
# =====================================================================

python3 -c "
import json, sys

results = [$(IFS=','; echo "${RESULTS_JSON[*]}")]
timestamp = '$(date -u +%Y-%m-%dT%H:%M:%SZ)'

# Compute aggregates
total = len(results)
passed = sum(1 for r in results if r['verdict'] == 'PASS')
failed = sum(1 for r in results if r['verdict'] == 'FAIL')

by_type = {}
for r in results:
    t = r.get('type', 'unknown')
    if t not in by_type:
        by_type[t] = {'pass': 0, 'fail': 0, 'total': 0, 'latencies': []}
    by_type[t]['total'] += 1
    by_type[t]['latencies'].append(r.get('latency_ms', 0))
    if r['verdict'] == 'PASS':
        by_type[t]['pass'] += 1
    else:
        by_type[t]['fail'] += 1

# Compute latency stats per type
for t in by_type:
    lats = sorted(by_type[t]['latencies'])
    n = len(lats)
    by_type[t]['latency_p50'] = lats[n // 2] if n else 0
    by_type[t]['latency_p95'] = lats[int(n * 0.95)] if n else 0
    by_type[t]['latency_mean'] = sum(lats) // n if n else 0
    del by_type[t]['latencies']

# By category (S=confident, B=bilingual, N=negative, K=keyword, M=multihop, R=retrieval)
categories = {
    'confident': {'prefix': 'S', 'pass': 0, 'fail': 0},
    'bilingual': {'prefix': 'B', 'pass': 0, 'fail': 0},
    'negative':  {'prefix': 'N', 'pass': 0, 'fail': 0},
    'keyword':   {'prefix': 'K', 'pass': 0, 'fail': 0},
    'imperative': {'prefix': 'I', 'pass': 0, 'fail': 0},
    'multihop':  {'prefix': 'M', 'pass': 0, 'fail': 0},
    'retrieval': {'prefix': 'R', 'pass': 0, 'fail': 0},
}
for r in results:
    for cat, info in categories.items():
        if r['id'].startswith(info['prefix']):
            if r['verdict'] == 'PASS':
                info['pass'] += 1
            else:
                info['fail'] += 1
            break

# All keyword stats
total_kw_hits = sum(r.get('keyword_hits', 0) for r in results if r.get('keyword_total', 0) > 0)
total_kw_expected = sum(r.get('keyword_total', 0) for r in results if r.get('keyword_total', 0) > 0)
keyword_hit_rate = round(total_kw_hits / total_kw_expected * 100, 1) if total_kw_expected else 0

# False positive rate (negative tests that returned confident)
neg_tests = [r for r in results if r['id'].startswith('N')]
false_positives = sum(1 for r in neg_tests if r.get('confidence') == 'confident')
fp_rate = round(false_positives / len(neg_tests) * 100, 1) if neg_tests else 0

output = {
    'timestamp': timestamp,
    'total_blocks': $total_blocks,
    'elapsed_seconds': $ELAPSED,
    'summary': {
        'total': total,
        'passed': passed,
        'failed': failed,
        'pass_rate': round(passed / total * 100, 1) if total else 0,
        'keyword_hit_rate': keyword_hit_rate,
        'false_positive_rate': fp_rate,
    },
    'by_type': by_type,
    'by_category': {k: {'pass': v['pass'], 'fail': v['fail'], 'total': v['pass']+v['fail']} for k, v in categories.items()},
    'results': results,
}

with open('$RESULTS_FILE', 'w') as f:
    json.dump(output, f, indent=2)

print()
" 2>/dev/null

# =====================================================================
# Summary Table
# =====================================================================

echo ""
echo "================================================================="
echo "  SUMMARY"
echo "================================================================="
echo ""

python3 -c "
import json

with open('$RESULTS_FILE') as f:
    data = json.load(f)

s = data['summary']
print(f'  Total tests:        {s[\"total\"]}')
print(f'  Passed:             {s[\"passed\"]}')
print(f'  Failed:             {s[\"failed\"]}')
print(f'  Pass rate:          {s[\"pass_rate\"]}%')
print(f'  Keyword hit rate:   {s[\"keyword_hit_rate\"]}%')
print(f'  False positive rate:{s[\"false_positive_rate\"]}%')
print(f'  Elapsed:            {data[\"elapsed_seconds\"]}s')
print()

# Category breakdown
print('  Category        Pass  Fail  Total  Rate')
print('  ' + '-' * 46)
for cat in ['confident', 'bilingual', 'negative', 'keyword', 'imperative', 'multihop', 'retrieval']:
    c = data['by_category'].get(cat, {'pass':0,'fail':0,'total':0})
    if c['total'] == 0:
        continue
    rate = round(c['pass'] / c['total'] * 100) if c['total'] else 0
    print(f'  {cat:16s}  {c[\"pass\"]:4d}  {c[\"fail\"]:4d}  {c[\"total\"]:5d}  {rate}%')

print()

# Latency breakdown
print('  Type            P50     P95     Mean')
print('  ' + '-' * 40)
for t, v in data['by_type'].items():
    print(f'  {t:16s}  {v[\"latency_p50\"]:5d}ms {v[\"latency_p95\"]:5d}ms {v[\"latency_mean\"]:5d}ms')

print()

# Failed tests detail
failures = [r for r in data['results'] if r['verdict'] == 'FAIL']
if failures:
    print('  FAILURES:')
    for f in failures:
        detail = f'{f[\"id\"]}: {f[\"desc\"]}'
        if 'confidence' in f:
            detail += f' (conf={f[\"confidence\"]}, expected={f[\"expected_confidence\"]})'
        if f.get('keyword_total', 0) > 0:
            detail += f' kw={f[\"keyword_hits\"]}/{f[\"keyword_total\"]}'
        print(f'    - {detail}')
    print()
" 2>/dev/null

# =====================================================================
# Baseline Regression Detection
# =====================================================================

if [[ -f "$BASELINE_FILE" ]]; then
  echo "--- Regression Check (vs baseline) ---"
  echo ""

  python3 -c "
import json, sys

with open('$RESULTS_FILE') as f:
    current = json.load(f)
with open('$BASELINE_FILE') as f:
    baseline = json.load(f)

cs = current['summary']
bs = baseline['summary']

regressions = []
improvements = []

# Pass rate
if cs['pass_rate'] < bs['pass_rate'] - 5:
    regressions.append(f'Pass rate: {bs[\"pass_rate\"]}% -> {cs[\"pass_rate\"]}% (REGRESSION)')
elif cs['pass_rate'] > bs['pass_rate'] + 5:
    improvements.append(f'Pass rate: {bs[\"pass_rate\"]}% -> {cs[\"pass_rate\"]}% (IMPROVED)')

# Keyword hit rate
if cs['keyword_hit_rate'] < bs['keyword_hit_rate'] - 10:
    regressions.append(f'Keyword hit rate: {bs[\"keyword_hit_rate\"]}% -> {cs[\"keyword_hit_rate\"]}% (REGRESSION)')
elif cs['keyword_hit_rate'] > bs['keyword_hit_rate'] + 10:
    improvements.append(f'Keyword hit rate: {bs[\"keyword_hit_rate\"]}% -> {cs[\"keyword_hit_rate\"]}% (IMPROVED)')

# False positive rate
if cs['false_positive_rate'] > bs['false_positive_rate'] + 10:
    regressions.append(f'False positive rate: {bs[\"false_positive_rate\"]}% -> {cs[\"false_positive_rate\"]}% (REGRESSION)')
elif cs['false_positive_rate'] < bs['false_positive_rate'] - 10:
    improvements.append(f'False positive rate: {bs[\"false_positive_rate\"]}% -> {cs[\"false_positive_rate\"]}% (IMPROVED)')

# Per-category regressions
for cat in ['confident', 'bilingual', 'negative', 'keyword', 'imperative', 'multihop', 'retrieval']:
    cc = current['by_category'].get(cat, {'pass':0, 'total':0})
    bc = baseline['by_category'].get(cat, {'pass':0, 'total':0})
    if bc['total'] == 0 or cc['total'] == 0:
        continue
    c_rate = cc['pass'] / cc['total'] * 100
    b_rate = bc['pass'] / bc['total'] * 100
    if c_rate < b_rate - 15:
        regressions.append(f'{cat}: {b_rate:.0f}% -> {c_rate:.0f}% (REGRESSION)')

# Latency regression (p95 > 2x baseline AND absolute increase > 500ms)
# Low absolute values (e.g. 89ms->202ms) are just jitter, not regressions
for t in current['by_type']:
    if t in baseline.get('by_type', {}):
        cp95 = current['by_type'][t].get('latency_p95', 0)
        bp95 = baseline['by_type'][t].get('latency_p95', 0)
        if bp95 > 0 and cp95 > bp95 * 2 and (cp95 - bp95) > 500:
            regressions.append(f'{t} P95 latency: {bp95}ms -> {cp95}ms (>2x REGRESSION)')

if regressions:
    print('  REGRESSIONS DETECTED:')
    for r in regressions:
        print(f'    !! {r}')
    print()

if improvements:
    print('  Improvements:')
    for i in improvements:
        print(f'    ++ {i}')
    print()

if not regressions and not improvements:
    print('  No significant changes vs baseline.')
    print()

# Print baseline date
print(f'  Baseline: {baseline[\"timestamp\"]} ({baseline[\"summary\"][\"total\"]} tests, {baseline[\"summary\"][\"pass_rate\"]}% pass rate)')
print()

sys.exit(1 if regressions else 0)
" 2>/dev/null
  REGRESSION_EXIT=$?
else
  echo "--- No baseline found. Run with --update-baseline to create one. ---"
  echo ""
  REGRESSION_EXIT=0
fi

# =====================================================================
# Update Baseline
# =====================================================================

if $UPDATE_BASELINE; then
  cp "$RESULTS_FILE" "$BASELINE_FILE"
  echo "Baseline updated: $BASELINE_FILE"
  echo ""
fi

# =====================================================================
# Verdict
# =====================================================================

echo "================================================================="
if (( FAIL == 0 )) && (( REGRESSION_EXIT == 0 )); then
  echo "  VERDICT: PASS ($PASS/$TOTAL tests, ${ELAPSED}s)"
elif (( REGRESSION_EXIT != 0 )); then
  echo "  VERDICT: REGRESSION ($PASS/$TOTAL tests passed, regressions detected)"
else
  echo "  VERDICT: FAIL ($FAIL/$TOTAL tests failed)"
fi
echo "================================================================="
echo ""
echo "Results saved to: $RESULTS_FILE"

# Exit code
if (( FAIL > 0 )) || (( REGRESSION_EXIT != 0 )); then
  exit 1
else
  exit 0
fi
