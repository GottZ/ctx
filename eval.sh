#!/usr/bin/env bash
#
# Context Store Evaluation Harness
# Usage: bash eval.sh                  — full eval (retrieval + synthesis)
#        bash eval.sh --retrieval-only  — skip synthesis tests (faster)
#        bash eval.sh --update-baseline — run + save results as new baseline
#        bash eval.sh --no-warmup       — skip the unscored warm-up pass (development runs)
#        bash eval.sh --internal        — bypass reverse-proxy (uses http://ctx:8080 from n8nintern network; override via CTX_INTERNAL_URL)
#
# Requires: curl, python3, jq (optional, python3 fallback)
# Runtime: ~6-10 minutes — the warm-up pass fires the same queries as the scored
#          pass, so the default run costs roughly twice a single pass.
#          ~3-5 minutes with --no-warmup (Ollama on-prem, sequential queries).
#
# ctx — Your AI's save game. By GottZ (github.com/GottZ/ctx/graphs/contributors)
# Evaluates GottZ 4-Way RRF retrieval and synthesis across 47 test queries.
# Source: https://github.com/GottZ/ctx

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${SCRIPT_DIR}/.env"

# =====================================================================
# Negative-case matcher (expected_conf == "none")
# =====================================================================
#
# Defined up here, before the .env preflight, and followed by a sourcing guard
# so eval-matcher_test.sh can `source eval.sh` to reach these two functions
# without running the harness, without .env and without firing a single query.
#
# The contract is the server's, read off the code — not a guessed enum:
#   * The confidence enum is confident | low_confidence |
#     no_relevant_blocks_found (go/internal/llm/synthesize.go:123-125).
#     "low" was never emitted by any server version this harness talks to and
#     is therefore gone.
#   * A refusal IS the exact text llm.NoRelevantReplacement,
#     "I don't know based on the available sources." (synthesize.go:49). The
#     server decides "the LLM rejected" with strings.HasPrefix(answer,
#     NoRelevantReplacement) (ApplyConfidenceOverride, synthesize.go:841); this
#     matcher mirrors that predicate so harness and server agree on what a
#     refusal is.
#   * low_confidence on its own is NOT a refusal: ClassifyConfidence
#     (synthesize.go:275-283) returns it for ANY mid-scoring query, a fully
#     answered one included. Accepting it unconditionally would leave the
#     negative class unable to notice a retrievable type that turns refusals
#     into answers — which is the one thing the negative class is for.

# NEGATIVE_REFUSAL_TEXT is llm.NoRelevantReplacement, lowercased: run_synthesis_test
# extracts the answer with .lower() (see below), so the comparison folds case.
NEGATIVE_REFUSAL_TEXT="i don't know based on the available sources."

# answer_is_refusal <answer> — true when the answer STARTS with the server's
# rejection text. Leading whitespace is tolerated (the query heartbeat prefixes
# the JSON body with spaces, go/internal/handler/query.go:484); apart from case
# folding the comparison is byte-exact — no regex, no fuzzy match.
answer_is_refusal() {
  local answer="$1" lowered
  answer="${answer#"${answer%%[![:space:]]*}"}"
  lowered=$(printf '%s' "$answer" | tr '[:upper:]' '[:lower:]')
  [[ "$lowered" == "$NEGATIVE_REFUSAL_TEXT"* ]]
}

# negative_conf_ok <confidence> <answer> — verdict for an expected-"none" case.
negative_conf_ok() {
  local confidence="$1" answer="$2"

  case "$confidence" in
    none|no_relevant_blocks_found|error|timeout)
      # none: legacy/other servers. error|timeout: harness-side, the request
      # produced no usable answer — unchanged from before.
      return 0
      ;;
    low_confidence)
      # Decisive, no regex rescue: only the real rejection text passes.
      if answer_is_refusal "$answer"; then
        return 0
      fi
      return 1
      ;;
  esac

  # Any other confidence (in practice: confident). Rescue markers carried over
  # unchanged from the pre-V-W2 matcher, plus the real rejection text.
  # NOTE: "not relevant" is a phrase a genuine answer can contain, so this
  # branch can wave one through; it is kept because narrowing it is a separate
  # change (see reports/bau/v-w2.md).
  if printf '%s\n' "$answer" | grep -qi "$NEGATIVE_REFUSAL_TEXT\|no_relevant_blocks_found\|keine relevanten\|cannot answer\|not relevant\|keine antwort"; then
    return 0
  fi
  return 1
}

# =====================================================================
# Instrumentation (Welle M-W6)
# =====================================================================
#
# Three things the harness could not show before, all defined up here — above
# the sourcing guard — so eval-instrument_test.sh reaches them without .env,
# without a running server and without firing a single query:
#
#   1. EVAL_CATEGORIES — ONE category registry. It used to live in two places
#      (the aggregation dict and the regression check's category list), so a
#      test ID with a new prefix counted into NO category and was watched by NO
#      threshold. Both python blocks read it from the environment now.
#   2. eval_census_json — the per-block-type census of the corpus the run
#      measured against. Without it a pass-rate change is not attributable:
#      total_blocks alone cannot tell a new retrievable type from a real
#      retrieval regression.
#   3. eval_baseline_diff — what --update-baseline actually changed, written
#      next to the baseline BEFORE the cp. The cp stays; it is no longer
#      traceless (design/05 §4.6 (3)).

# Category registry: category name -> test-ID prefix. Insertion order is the
# display order of the summary table and the regression check.
#
# 'derived' (prefix G) is registered WITHOUT any test case on purpose. The
# cases arrive with the first derived level that goes live; until then the
# category is reported as EMPTY (total 0) rather than being silently absent —
# an unregistered prefix is the failure mode this registry exists to prevent.
export EVAL_CATEGORIES='{"confident":"S","bilingual":"B","negative":"N","keyword":"K","imperative":"I","multihop":"M","temporal":"T","retrieval":"R","derived":"G"}'

# eval_census_json — reads a /api/manage {"action":"stats"} response on STDIN
# and prints one line of JSON:
#   {"by_block_type": {type: count} | null, "population": int | null,
#    "census_source": "..."}
#
# by_block_type covers the RETRIEVABLE, non-archived types only — the
# population a query can actually reach — and the server decides which types
# those are (store.GetDriftCensus marks each row `retrievable` from the
# block-type registry snapshot, go/internal/store/drift.go:104-108). The
# harness deliberately does NOT re-derive that rule; a second opinion about
# retrieval visibility is exactly the drift this field is meant to expose.
#
# The census section is admin-gated and opt-in (handler/context_manage.go:790-806).
# Nothing here is fatal: a caller without admin rights, an unreachable server or
# a malformed response yields by_block_type=null plus the reason in
# census_source. The eval run itself is unaffected.
EVAL_CENSUS_PY=$(cat <<'PY'
import json, sys


def emit(by_type, population, source):
    json.dump({'by_block_type': by_type, 'population': population,
               'census_source': source}, sys.stdout)
    sys.stdout.write('\n')


raw = sys.stdin.read()
try:
    doc = json.loads(raw) if raw.strip() else None
except ValueError:
    doc = None
if not isinstance(doc, dict):
    emit(None, None, 'unavailable: stats response is not a JSON object')
    sys.exit(0)

drift = doc.get('drift')
if not isinstance(drift, dict):
    reason = doc.get('error') or 'stats response carries no drift section'
    emit(None, None, 'unavailable: %s' % str(reason)[:120])
    sys.exit(0)

by_type = {}
for row in drift.get('types') or []:
    if isinstance(row, dict) and row.get('retrievable'):
        by_type[row.get('type_name')] = row.get('count')
population = sum(v for v in by_type.values() if isinstance(v, int))
source = 'manage stats drift @ %s' % (drift.get('at') or 'unknown')

# The server sums the same rows itself; a disagreement means the census was
# read wrong and must not pass as a clean number.
server_total = drift.get('retrievable_blocks')
if isinstance(server_total, int) and server_total != population:
    source += ' (MISMATCH: server retrievable_blocks=%d)' % server_total
emit(by_type, population, source)
PY
)

# Reads the stats response on STDIN (python3 -c leaves stdin to the program).
eval_census_json() {
  python3 -c "$EVAL_CENSUS_PY"
}

# eval_baseline_diff <results-file> <baseline-file> <out-file> — writes the
# record of what a baseline update changes and prints a one-line digest.
#
# Called BEFORE the cp, because afterwards the old baseline no longer exists.
# With no previous baseline the out-file holds the single line
# "no previous baseline" — the file is written either way so a stale diff from
# an earlier update can never be mistaken for the current one.
EVAL_BASELINE_DIFF_PY=$(cat <<'PY'
import json, os, sys
from datetime import datetime, timezone

cur_path, base_path, out_path = sys.argv[1], sys.argv[2], sys.argv[3]

with open(cur_path) as fh:
    cur = json.load(fh)

if not os.path.exists(base_path):
    with open(out_path, 'w') as fh:
        fh.write('no previous baseline\n')
    print('  Baseline diff: no previous baseline')
    sys.exit(0)

with open(base_path) as fh:
    old = json.load(fh)

categories = json.loads(os.environ.get('EVAL_CATEGORIES') or '{}')


def verdicts(doc):
    return {r.get('id'): r.get('verdict') for r in doc.get('results') or []}


ov, cv = verdicts(old), verdicts(cur)
flips = [{'id': tid, 'from': ov.get(tid), 'to': cv.get(tid)}
         for tid in sorted(set(ov) | set(cv)) if ov.get(tid) != cv.get(tid)]
counts = {
    'pass_to_fail': sum(1 for f in flips if f['from'] == 'PASS' and f['to'] == 'FAIL'),
    'fail_to_pass': sum(1 for f in flips if f['from'] == 'FAIL' and f['to'] == 'PASS'),
    'added': sum(1 for f in flips if f['from'] is None),
    'removed': sum(1 for f in flips if f['to'] is None),
}

by_cat = {}
for name in categories:
    oc = (old.get('by_category') or {}).get(name) or {}
    cc = (cur.get('by_category') or {}).get(name) or {}
    by_cat[name] = {'from': [oc.get('pass', 0), oc.get('total', 0)],
                    'to': [cc.get('pass', 0), cc.get('total', 0)]}

ob, cb = old.get('by_block_type'), cur.get('by_block_type')
changed = {}
if isinstance(ob, dict) or isinstance(cb, dict):
    names = set((ob or {})) | set((cb or {}))
    changed = {n: [(ob or {}).get(n), (cb or {}).get(n)]
               for n in sorted(names) if (ob or {}).get(n) != (cb or {}).get(n)}

diff = {
    'at': datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ'),
    'baseline_file': base_path,
    'results_file': cur_path,
    'timestamps': {'from': old.get('timestamp'), 'to': cur.get('timestamp')},
    'summary': {'from': old.get('summary'), 'to': cur.get('summary')},
    'by_category': by_cat,
    'flips': flips,
    'flip_counts': counts,
    'by_block_type': {'from': ob, 'to': cb, 'changed': changed},
    'population': {'from': old.get('population'), 'to': cur.get('population')},
    'census_source': {'from': old.get('census_source'), 'to': cur.get('census_source')},
}
with open(out_path, 'w') as fh:
    json.dump(diff, fh, indent=2)
    fh.write('\n')

os_ = old.get('summary') or {}
cs_ = cur.get('summary') or {}


# A file written before the census existed has no population — say so, do not
# print a Python None into an operator's terminal.
def pop(v):
    return 'n/a' if v is None else v


line = '  Baseline diff: %s/%s -> %s/%s passed, %d flips (P->F %d, F->P %d), population %s -> %s' % (
    os_.get('passed'), os_.get('total'), cs_.get('passed'), cs_.get('total'),
    len(flips), counts['pass_to_fail'], counts['fail_to_pass'],
    pop(old.get('population')), pop(cur.get('population')))
print(line)
worse = [f['id'] for f in flips if f['from'] == 'PASS' and f['to'] == 'FAIL']
if worse:
    print('  Baseline diff: PASS->FAIL: %s' % ', '.join(worse))
PY
)

eval_baseline_diff() {
  python3 -c "$EVAL_BASELINE_DIFF_PY" "$1" "$2" "$3"
}

# eval_aggregate <results-array-file> <total_blocks> <elapsed_s> <timestamp> <out-file>
#   census object (eval_census_json output) on STDIN
#
# The whole results-JSON build. It reads the result records from a FILE instead
# of taking them through shell interpolation, which is what makes the golden
# test possible: an existing eval-results-*.json can be fed straight back
# through this function and must reproduce its own summary/by_category.
EVAL_AGGREGATE_PY=$(cat <<'PY'
import json, os, sys

array_path, total_blocks, elapsed, timestamp, out_path = sys.argv[1:6]

with open(array_path) as fh:
    results = json.load(fh)
raw_census = sys.stdin.read().strip()
census = json.loads(raw_census) if raw_census else {}

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

# By category — prefixes come from the ONE registry (EVAL_CATEGORIES).
categories = {name: {'prefix': prefix, 'pass': 0, 'fail': 0}
              for name, prefix in json.loads(os.environ['EVAL_CATEGORIES']).items()}
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

# Source-level assertion stats
src_checked = [r for r in results if r.get('source_total', 0) > 0]
src_total_patterns = sum(r['source_total'] for r in src_checked)
src_total_hits = sum(r['source_hits'] for r in src_checked)
src_tests_pass = sum(1 for r in src_checked if r['source_hits'] == r['source_total'])
src_hit_rate = round(src_total_hits / src_total_patterns * 100, 1) if src_total_patterns else 0

output = {
    'timestamp': timestamp,
    'total_blocks': int(total_blocks),
    # Corpus census of THIS run (M-W6): which retrievable types, how many
    # blocks each, and their sum as the population every rate below is a rate
    # OF. Null when the census was not reachable — census_source says why.
    'by_block_type': census.get('by_block_type'),
    'population': census.get('population'),
    'census_source': census.get('census_source', 'unavailable: no census taken'),
    'elapsed_seconds': int(elapsed),
    'summary': {
        'total': total,
        'passed': passed,
        'failed': failed,
        'pass_rate': round(passed / total * 100, 1) if total else 0,
        'keyword_hit_rate': keyword_hit_rate,
        'false_positive_rate': fp_rate,
        'source_checks': len(src_checked),
        'source_hit_rate': src_hit_rate,
    },
    'by_type': by_type,
    'by_category': {k: {'pass': v['pass'], 'fail': v['fail'], 'total': v['pass'] + v['fail']} for k, v in categories.items()},
    'results': results,
}

with open(out_path, 'w') as fh:
    json.dump(output, fh, indent=2)

print()
PY
)

# Census object on STDIN, everything else in argv.
eval_aggregate() {
  python3 -c "$EVAL_AGGREGATE_PY" "$@"
}

# Sourcing guard: everything below is the harness. `source eval.sh` returns
# here with the matcher and the instrumentation defined; direct execution falls
# through.
if [[ "${BASH_SOURCE[0]}" != "${0}" ]]; then
  return 0
fi

if [[ ! -f "$ENV_FILE" ]]; then
    echo "[FATAL] .env not found at $ENV_FILE"
    exit 1
fi
set -a; source "$ENV_FILE"; set +a

KEY_PRIVATE="${CONTEXT_API_KEY_PRIVATE:?CONTEXT_API_KEY_PRIVATE not set in .env}"
BASELINE_FILE="${SCRIPT_DIR}/.eval-baseline.json"
# Written by --update-baseline, next to the baseline it replaces (M-W6).
BASELINE_DIFF_FILE="${SCRIPT_DIR}/.eval-baseline.diff"
RESULTS_DIR="${SCRIPT_DIR}/.eval-results"
mkdir -p "$RESULTS_DIR"
RESULTS_FILE="${RESULTS_DIR}/eval-results-$(date +%s).json"

RETRIEVAL_ONLY=false
UPDATE_BASELINE=false
INTERNAL=false
WARMUP=true
for arg in "$@"; do
  case "$arg" in
    --retrieval-only) RETRIEVAL_ONLY=true ;;
    --update-baseline) UPDATE_BASELINE=true ;;
    --internal) INTERNAL=true ;;
    --no-warmup) WARMUP=false ;;
  esac
done

# --- Warm-up pass ---------------------------------------------------------
# A first run against a cold system reports flags that a second, identical run
# does not — the operating rule used to be "run eval.sh twice, score run 2".
# That rule lives here now: unless --no-warmup is given, every scored pass is
# preceded by an unscored pass over the *same* queries whose results are
# discarded (no verdicts, no counters, nothing in the results JSON).
#
# What the warm-up actually warms, as far as this repo can show:
#   * context_embed_cache — query embeddings are cached by (text_hash, model)
#     (go/internal/embedcache/embedcache.go), so the second pass over identical
#     query strings does not re-embed.
#   * PostgreSQL shared_buffers / OS page cache — the HNSW index
#     (idx_embedding_hnsw, go/migrations/115_*) and the block rows behind it are
#     read from disk on first touch.
# Anything else a cold first request pays for on the serving side is warmed as a
# side effect; this script does not measure it and makes no claim about it.
#
# SCORING gates the scoring half of the two test functions. During the warm-up
# the functions still issue their requests but return right after them, and the
# pass reports progress as one dot per test on FD 3 (see below).
SCORING=true

# FD 3 is the real stdout. The warm-up pass runs with stdout redirected to
# /dev/null so its per-test lines and section headings stay out of the report;
# the progress dots go to FD 3 and therefore end up wherever stdout points.
exec 3>&1

# Webhook selection: --internal bypasses the reverse-proxy that drops 22-67% of
# LLM responses (Welle 35). Uses the ctx container DNS-name resolvable from the
# n8nintern compose network. Override with CTX_INTERNAL_URL if needed. Caller
# must run from inside the n8nintern network (e.g. `docker run --rm --network
# n8nintern …` or from another container attached to n8nintern). Container-IPs
# are unstable across restarts (W47-NEU-F), DNS-name is stable.
if $INTERNAL; then
  WEBHOOK="${CTX_INTERNAL_URL:-http://ctx:8080}"
else
  WEBHOOK="${WEBHOOK_BASE_URL:-https://localhost}"
fi

# =====================================================================
# Helpers
# =====================================================================

# api <url> <body> [timeout] [key] — key defaults to the harness key, so every
# call that existed before this parameter behaves exactly as it did. Only the
# block-type census (M-W6) passes a different one: its stats section is
# admin-gated, and the harness key is not an admin key.
api() {
  local timeout="${3:-120}"
  local key="${4:-$KEY_PRIVATE}"
  curl -s --max-time "$timeout" -X POST "$1" \
    -H "Content-Type: application/json" \
    -H "X-Context-Key: $key" \
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
# Format: ID|TYPE|QUERY|EXPECTED_CONFIDENCE|KEYWORDS(comma-sep)|DESCRIPTION|SOURCE_TITLES(optional)
#
# TYPE: retrieval = api/search (no LLM), synthesis = api/query (LLM)
#       NOTE: the "retrieval" type name is a JSON key the baseline compares on and
#       therefore fixed, but it is NOT a measurement of the 4-Way RRF. The R-tests
#       hit /api/search → store.SearchBlocks; only the synthesis tests (/api/query)
#       go through ctx_rrf. Read the R-rows as search-endpoint quality, nothing more.
# EXPECTED_CONFIDENCE: confident|low|none|any (for retrieval tests)
# KEYWORDS: comma-separated strings that MUST appear in the answer (case-insensitive)
#           OR-alternatives: use ~ separator (e.g. "german~deutsch" matches either)
#           For retrieval tests: keywords that must appear in result titles/previews
# SOURCE_TITLES: comma-separated substrings that must appear in at least one source title
#                (case-insensitive, INFORMATIONAL only — does not affect PASS/FAIL)

define_test_cases() {
cat <<'CASES'
# --- CONFIDENT SYNTHESIS (known facts, should return confident) ---
S01|synthesis|What embedding model does the context store use?|confident|qwen3,embedding|Core infrastructure fact|Embedding-Strategie,Aktive Modelle
S02|synthesis|How does the Write Guard detect duplicates?|confident|hash,similarity,async|Architecture knowledge|Write Guard Architecture
S03|synthesis|What are the scope values in the multi-tenant system?|confident|private,work,shared|Scope enum values|Scope-Architektur
S04|synthesis|What is the PostgreSQL version and what extension is used for vectors?|confident|18,pgvector|DB stack|pgvector
S05|synthesis|What LLM model is used for synthesis in the context-agent?|confident|qwen3,9b|Model config|Aktive Modelle
S06|synthesis|How does the blob storage authenticate requests?|confident|key,hash,sha|Auth mechanism|
S07|synthesis|What is the RRF fusion strategy used in retrieval?|confident|rrf,fusion|Retrieval architecture|RRF
S08|synthesis|What are the Write Guard similarity thresholds?|confident|0.98,0.92|Threshold values|Write Guard Architecture
S09|synthesis|What happened with the qwen3.5:9b model evaluation?|confident|death,spiral,thinking,token|Known failure case|
S10|synthesis|What is the Matryoshka truncation dimension for embeddings?|confident|1024|Embedding dimension|Embedding Dimension,Embedding-Strategie

# --- BILINGUAL (German queries about English-titled content) ---
B01|synthesis|Welches Embedding-Modell wird verwendet?|confident|qwen3,embedding|DE query, EN content|Embedding-Strategie
B02|synthesis|Wie funktioniert der Write Guard?|confident|hash,similarity~ähnlichkeit|DE query about guard|Write Guard
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
M03|synthesis|What are the Write Guard similarity thresholds and what action happens at each level?|confident|0.98,0.92,archive~auto-archive,review~needs_review~flag|Guard threshold actions
M04|synthesis|What are the differences between context-search and context-agent?|confident|search,agent,llm,compact|Endpoint comparison
M05|synthesis|How does the bilingual retrieval gap affect German queries and what is the fix?|confident|german~deutsch~deutschen,translation~uebersetzung~übersetzung,retrieval|Problem + solution

# --- IMPERATIVE (command-style queries, should NOT be confused with keywords) ---
I01|synthesis|Zeig mir die Write Guard Thresholds|confident|0.98,0.92|DE imperative, known thresholds
I02|synthesis|List all scope values|confident|private,shared|EN imperative, enum values
I03|synthesis|Nenn die Modelle die im System laufen|confident|qwen3,embedding|DE imperative, model inventory
I04|synthesis|Describe the RRF retrieval strategy|confident|semantic,rrf,weight|EN imperative, architecture
I05|synthesis|Zeig Scopes|confident|private,shared|Minimal 2-word DE imperative
I06|synthesis|Show me how auth works in the context store|confident|key,hash,scope|EN imperative, auth flow
I07|synthesis|Liste alle Kategorien im Context Store auf|confident|infrastructure,learnings|DE imperative, categories
I08|synthesis|Explain the blob storage authentication|confident|key,hash,blob|EN imperative, blob auth

# --- TEMPORAL (queries with temporal references — tests FTS expansion + Gravity Boost) ---
T01|synthesis|Was wurde letzte Woche im Context Store geändert?|confident|block~store~context|Relative week reference
T02|synthesis|Welche Architektur-Entscheidungen wurden im März 2026 getroffen?|confident|rrf~guard~embedding~scope|Month+year reference
T03|synthesis|What embedding changes happened recently?|confident|embedding~embed|EN temporal, recent
T04|synthesis|What happened at night during the infrastructure work?|any||LLM fallback: night triggers intent but escapes all matchers (confidence varies)

# --- SEARCH-ENDPOINT QUALITY (api/search, no LLM — NOT ctx_rrf, see note above) ---
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
SOURCE_CHECKS=0
SOURCE_HITS=0
SOURCE_MISSES=0
declare -a RESULTS_JSON=()
START_TIME=$(date +%s)

run_synthesis_test() {
  local id="$1" query="$2" expected_conf="$3" keywords="$4" desc="$5"
  local expected_sources="${6:-}"
  local t_start t_end latency_ms resp answer confidence sources_count keyword_hits keyword_total keyword_pct verdict detail

  t_start=$(date +%s%3N)
  local escaped_query
  escaped_query=$(printf '%s' "$query" | python3 -c "import sys,json; print(json.dumps(sys.stdin.read()))")
  resp=$(api "$WEBHOOK/api/query" "{\"query\":$escaped_query}" 120)
  t_end=$(date +%s%3N)
  latency_ms=$(( t_end - t_start ))

  # Warm-up pass: the request was the point, the response is discarded. Return
  # before any scoring so counters and RESULTS_JSON stay untouched.
  if ! $SCORING; then printf '.' >&3; return 0; fi

  # Parse response — handle timeouts and error responses
  local source_titles_raw=""
  if [[ -z "$resp" ]]; then
    answer=""
    confidence="timeout"
    sources_count=0
  else
    answer=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('answer','').lower())" 2>/dev/null || echo "")
    confidence=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('confidence','error'))" 2>/dev/null || echo "error")
    sources_count=$(echo "$resp" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('sources',[])))" 2>/dev/null || echo "0")
    # Extract source titles (newline-separated, lowercased)
    source_titles_raw=$(echo "$resp" | python3 -c "
import sys,json
d=json.load(sys.stdin)
for s in d.get('sources',[]):
    print(s.get('title','').lower())
" 2>/dev/null || echo "")
  fi

  # Check confidence
  local conf_ok=false
  if [[ "$expected_conf" == "any" ]]; then
    conf_ok=true
  elif [[ "$expected_conf" == "none" ]]; then
    # Negative tests: the server's real contract, not a guessed enum.
    # See negative_conf_ok at the top of this file (and eval-matcher_test.sh).
    if negative_conf_ok "$confidence" "$answer"; then
      conf_ok=true
    fi
  elif [[ "$confidence" == "$expected_conf" ]]; then
    conf_ok=true
  fi

  # Check keywords. Decimal commas are normalized to dots first: German answers
  # write "0,98" for 0.98, which silently misses numeric keywords (I01).
  local answer_kw
  answer_kw=$(echo "$answer" | sed -E 's/([0-9]),([0-9])/\1.\2/g')
  keyword_hits=0
  keyword_total=0
  if [[ -n "$keywords" ]]; then
    IFS=',' read -ra KW_ARRAY <<< "$keywords"
    keyword_total=${#KW_ARRAY[@]}
    for kw_group in "${KW_ARRAY[@]}"; do
      IFS='~' read -ra KW_ALTS <<< "$kw_group"
      local found=false
      for alt in "${KW_ALTS[@]}"; do
        if echo "$answer_kw" | grep -qiF "$alt"; then
          found=true
          break
        fi
      done
      if $found; then
        keyword_hits=$(( keyword_hits + 1 ))
      fi
    done
  fi

  if (( keyword_total > 0 )); then
    keyword_pct=$(( keyword_hits * 100 / keyword_total ))
  else
    keyword_pct=100  # no keywords to check = pass
  fi

  # Check source titles (INFORMATIONAL — does not affect verdict)
  local src_hits=0 src_total=0 src_verdict="" src_missing=""
  if [[ -n "$expected_sources" ]]; then
    IFS=',' read -ra SRC_ARRAY <<< "$expected_sources"
    src_total=${#SRC_ARRAY[@]}
    for src_pattern in "${SRC_ARRAY[@]}"; do
      local src_lower
      src_lower=$(echo "$src_pattern" | tr '[:upper:]' '[:lower:]')
      if echo "$source_titles_raw" | grep -qiF "$src_lower"; then
        src_hits=$(( src_hits + 1 ))
      else
        [[ -n "$src_missing" ]] && src_missing="$src_missing, "
        src_missing="${src_missing}${src_pattern}"
      fi
    done
    SOURCE_CHECKS=$(( SOURCE_CHECKS + 1 ))
    if (( src_hits == src_total )); then
      SOURCE_HITS=$(( SOURCE_HITS + 1 ))
      src_verdict="OK"
    else
      SOURCE_MISSES=$(( SOURCE_MISSES + 1 ))
      src_verdict="MISS"
    fi
  fi

  # Verdict (source check is informational, does NOT affect pass/fail)
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
  printf "%s %-4s %-50s conf=%-10s kw=%d/%d" \
    "$status_icon" "$id" "${desc:0:50}" "$confidence" "$keyword_hits" "$keyword_total"
  if (( src_total > 0 )); then
    printf "  src=%d/%d" "$src_hits" "$src_total"
  fi
  printf "  %5dms" "$latency_ms"
  [[ -n "$detail" ]] && printf "  !! %s" "$detail"
  if [[ "$src_verdict" == "MISS" ]]; then
    printf "  ?? src-miss: %s" "$src_missing"
  fi
  echo ""

  # Store result as JSON line (include source check data)
  local src_json="\"source_hits\":$src_hits,\"source_total\":$src_total"
  RESULTS_JSON+=("{\"id\":\"$id\",\"type\":\"synthesis\",\"verdict\":\"$verdict\",\"confidence\":\"$confidence\",\"expected_confidence\":\"$expected_conf\",\"keyword_hits\":$keyword_hits,\"keyword_total\":$keyword_total,\"keyword_pct\":$keyword_pct,$src_json,\"latency_ms\":$latency_ms,\"sources\":$sources_count,\"desc\":\"$desc\"}")
}

run_retrieval_test() {
  local id="$1" query="$2" expected_conf="$3" keywords="$4" desc="$5"
  local t_start t_end latency_ms resp count titles keyword_hits keyword_total keyword_pct verdict detail

  t_start=$(date +%s%3N)
  local escaped_query
  escaped_query=$(printf '%s' "$query" | python3 -c "import sys,json; print(json.dumps(sys.stdin.read()))")
  resp=$(api "$WEBHOOK/api/search" "{\"query\":$escaped_query}" 30)
  t_end=$(date +%s%3N)
  latency_ms=$(( t_end - t_start ))

  # Warm-up pass: the request was the point, the response is discarded. Return
  # before any scoring so counters and RESULTS_JSON stay untouched.
  if ! $SCORING; then printf '.' >&3; return 0; fi

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
    for kw_group in "${KW_ARRAY[@]}"; do
      IFS='~' read -ra KW_ALTS <<< "$kw_group"
      local found=false
      for alt in "${KW_ALTS[@]}"; do
        if echo "$titles" | grep -qiF "$alt"; then
          found=true
          break
        fi
      done
      if $found; then
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

# --- Config ---
TEST_COUNT=$(define_test_cases | grep -cE '^[A-Z][0-9]+\|')
echo "Config: webhook=${WEBHOOK}"
echo "Config: embed_model=${OLLAMA_EMBED_MODEL:-unset}"
echo "Config: embed_dims=${OLLAMA_EMBED_DIMS:-unset}"
echo "Config: chat_model=${OLLAMA_CHAT_MODEL:-unset}"
echo "Config: tests=${TEST_COUNT}"
echo ""

# Preflight: verify connectivity
echo "--- Preflight ---"
preflight=$(api "$WEBHOOK/api/manage" '{"action":"stats"}' 15)
total_blocks=$(echo "$preflight" | python3 -c "import sys,json; print(json.load(sys.stdin)['stats']['total_blocks'])" 2>/dev/null || echo "0")
if (( total_blocks < 100 )); then
  echo "ABORT: Context store has only $total_blocks blocks (expected 100+). Is the system up?"
  exit 1
fi
echo "Store OK: $total_blocks blocks"

# Block-type census (M-W6), taken ONCE per run like total_blocks and in a
# SEPARATE request: the drift section is admin-gated, and a non-admin key asking
# for it gets a 403 for the whole stats call (handler/context_manage.go:804) —
# which would take the preflight down with it. CTX_ADMIN_KEY comes out of the
# same .env this script already sources; where it is absent the harness key is
# tried (it is the admin key on some installs), and where that fails too the
# run continues with by_block_type=null and the reason recorded in the results
# file. No new secret has to be provisioned for an eval run.
census=$(api "$WEBHOOK/api/manage" '{"action":"stats","data":{"drift":true}}' 60 "${CTX_ADMIN_KEY:-$KEY_PRIVATE}")
CENSUS_JSON=$(printf '%s' "$census" | eval_census_json)
echo "Census: $(printf '%s' "$CENSUS_JSON" | python3 -c "
import sys, json
d = json.load(sys.stdin)
bt = d.get('by_block_type')
if bt is None:
    print(d.get('census_source'))
else:
    print('%d retrievable blocks in %d types (%s)' % (d.get('population') or 0, len(bt), ', '.join(sorted(bt))))
" 2>/dev/null || echo "unavailable")"
echo ""

run_all_tests() {
  # --- Search-endpoint tests ---
  # These five R-tests measure /api/search (store.SearchBlocks), NOT ctx_rrf.
  echo "--- Search-endpoint Tests (api/search, no LLM — not ctx_rrf) ---"
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
    echo "--- Synthesis Tests (api/query, LLM) ---"
    echo ""

    # Confident
    echo "  -- Confident (known facts) --"
    while IFS='|' read -r id type query expected_conf keywords desc source_titles; do
      [[ "$id" =~ ^#.*$ ]] && continue
      [[ -z "$id" ]] && continue
      [[ "$type" != "synthesis" ]] && continue
      [[ ! "$id" =~ ^S ]] && continue
      run_synthesis_test "$id" "$query" "$expected_conf" "$keywords" "$desc" "$source_titles"
    done < <(define_test_cases)

    echo ""
    echo "  -- Bilingual (DE query, EN content) --"
    while IFS='|' read -r id type query expected_conf keywords desc source_titles; do
      [[ "$id" =~ ^#.*$ ]] && continue
      [[ -z "$id" ]] && continue
      [[ "$type" != "synthesis" ]] && continue
      [[ ! "$id" =~ ^B ]] && continue
      run_synthesis_test "$id" "$query" "$expected_conf" "$keywords" "$desc" "$source_titles"
    done < <(define_test_cases)

    echo ""
    echo "  -- Negative (should reject) --"
    while IFS='|' read -r id type query expected_conf keywords desc source_titles; do
      [[ "$id" =~ ^#.*$ ]] && continue
      [[ -z "$id" ]] && continue
      [[ "$type" != "synthesis" ]] && continue
      [[ ! "$id" =~ ^N ]] && continue
      run_synthesis_test "$id" "$query" "$expected_conf" "$keywords" "$desc" "$source_titles"
    done < <(define_test_cases)

    echo ""
    echo "  -- Keyword / Specific Facts --"
    while IFS='|' read -r id type query expected_conf keywords desc source_titles; do
      [[ "$id" =~ ^#.*$ ]] && continue
      [[ -z "$id" ]] && continue
      [[ "$type" != "synthesis" ]] && continue
      [[ ! "$id" =~ ^K ]] && continue
      run_synthesis_test "$id" "$query" "$expected_conf" "$keywords" "$desc" "$source_titles"
    done < <(define_test_cases)

    echo ""
    echo "  -- Imperative (command-style queries) --"
    while IFS='|' read -r id type query expected_conf keywords desc source_titles; do
      [[ "$id" =~ ^#.*$ ]] && continue
      [[ -z "$id" ]] && continue
      [[ "$type" != "synthesis" ]] && continue
      [[ ! "$id" =~ ^I ]] && continue
      run_synthesis_test "$id" "$query" "$expected_conf" "$keywords" "$desc" "$source_titles"
    done < <(define_test_cases)

    echo ""
    echo "  -- Multi-hop (cross-block reasoning) --"
    while IFS='|' read -r id type query expected_conf keywords desc source_titles; do
      [[ "$id" =~ ^#.*$ ]] && continue
      [[ -z "$id" ]] && continue
      [[ "$type" != "synthesis" ]] && continue
      [[ ! "$id" =~ ^M ]] && continue
      run_synthesis_test "$id" "$query" "$expected_conf" "$keywords" "$desc" "$source_titles"
    done < <(define_test_cases)

    echo ""
    echo "  -- Temporal (temporal references) --"
    while IFS='|' read -r id type query expected_conf keywords desc source_titles; do
      [[ "$id" =~ ^#.*$ ]] && continue
      [[ -z "$id" ]] && continue
      [[ "$type" != "synthesis" ]] && continue
      [[ ! "$id" =~ ^T ]] && continue
      run_synthesis_test "$id" "$query" "$expected_conf" "$keywords" "$desc" "$source_titles"
    done < <(define_test_cases)
  fi
}

# --- Warm-up pass (not scored) ---
# Same queries, same endpoints, results thrown away. See the SCORING block near
# the top for what this warms and why the rule lives in the script now.
if $WARMUP; then
  echo "--- warm-up pass (not scored) ---"
  warmup_start=$(date +%s)
  SCORING=false
  run_all_tests >/dev/null
  SCORING=true
  echo ""
  echo "warm-up done in $(( $(date +%s) - warmup_start ))s — results discarded, scoring starts now."
  echo ""
else
  echo "--- warm-up pass SKIPPED (--no-warmup) — cold-start flags are expected ---"
  echo ""
fi

# The scored pass. START_TIME is re-armed here so elapsed_seconds keeps meaning
# "duration of the scored pass" and stays comparable with older baselines.
START_TIME=$(date +%s)
run_all_tests

END_TIME=$(date +%s)
ELAPSED=$(( END_TIME - START_TIME ))

# =====================================================================
# Results JSON
# =====================================================================

# The result records go through a FILE instead of shell interpolation, which
# is what makes the aggregation a function eval-instrument_test.sh can call
# (M-W6). The census object rides in on stdin; stderr stays silenced exactly
# as it was when this block was inline.
RESULTS_ARRAY_FILE="${RESULTS_DIR}/.results-array-$$.json"
printf '[%s]' "$(IFS=','; echo "${RESULTS_JSON[*]}")" > "$RESULTS_ARRAY_FILE"
printf '%s' "$CENSUS_JSON" | eval_aggregate "$RESULTS_ARRAY_FILE" "$total_blocks" \
  "$ELAPSED" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$RESULTS_FILE" 2>/dev/null
rm -f "$RESULTS_ARRAY_FILE"

# =====================================================================
# Summary Table
# =====================================================================

echo ""
echo "================================================================="
echo "  SUMMARY"
echo "================================================================="
echo ""

python3 -c "
import json, os

with open('$RESULTS_FILE') as f:
    data = json.load(f)

s = data['summary']
print(f'  Total tests:        {s[\"total\"]}')
print(f'  Passed:             {s[\"passed\"]}')
print(f'  Failed:             {s[\"failed\"]}')
print(f'  Pass rate:          {s[\"pass_rate\"]}%')
print(f'  Keyword hit rate:   {s[\"keyword_hit_rate\"]}%')
print(f'  False positive rate:{s[\"false_positive_rate\"]}%')
if s.get('source_checks', 0) > 0:
    print(f'  Source checks:      {s[\"source_checks\"]} tests, {s[\"source_hit_rate\"]}% pattern hit rate')
print(f'  Elapsed:            {data[\"elapsed_seconds\"]}s')
# The population every rate above is a rate OF (M-W6, Masterplan K9).
if data.get('population') is not None:
    print(f'  Population:         {data[\"population\"]} retrievable of {data[\"total_blocks\"]} blocks')
else:
    print(f'  Population:         unavailable — {data.get(\"census_source\")}')
print()

# Display labels. The dict KEYS ('retrieval', by_type keys, test IDs) are what the
# baseline compares on and stay as they are; only what a human reads is renamed.
# 'retrieval' measures /api/search (store.SearchBlocks), not ctx_rrf — the old
# label invited exactly the reading these five tests cannot support.
LABELS = {'retrieval': 'search-endpt'}

# Category breakdown — order comes from the ONE registry (EVAL_CATEGORIES).
print('  Category        Pass  Fail  Total  Rate')
print('  ' + '-' * 46)
for cat in json.loads(os.environ['EVAL_CATEGORIES']):
    c = data['by_category'].get(cat, {'pass':0,'fail':0,'total':0})
    if c['total'] == 0:
        continue
    rate = round(c['pass'] / c['total'] * 100) if c['total'] else 0
    print(f'  {LABELS.get(cat, cat):16s}  {c[\"pass\"]:4d}  {c[\"fail\"]:4d}  {c[\"total\"]:5d}  {rate}%')

print()
print('  search-endpt = the five R-tests against /api/search (store.SearchBlocks).')
print('  They do NOT exercise ctx_rrf; the synthesis rows above do.')
print()

# Latency breakdown
print('  Type            P50     P95     Mean')
print('  ' + '-' * 40)
for t, v in data['by_type'].items():
    print(f'  {LABELS.get(t, t):16s}  {v[\"latency_p50\"]:5d}ms {v[\"latency_p95\"]:5d}ms {v[\"latency_mean\"]:5d}ms')

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

# Source misses (informational)
src_misses = [r for r in data['results'] if r.get('source_total', 0) > 0 and r['source_hits'] < r['source_total']]
if src_misses:
    print('  SOURCE MISSES (informational):')
    for r in src_misses:
        print(f'    ?? {r[\"id\"]}: {r[\"desc\"]} (src={r[\"source_hits\"]}/{r[\"source_total\"]})')
    print()
" 2>/dev/null

# =====================================================================
# Baseline Regression Detection
# =====================================================================

if [[ -f "$BASELINE_FILE" ]]; then
  echo "--- Regression Check (vs baseline) ---"
  echo ""

  python3 -c "
import json, os, sys

with open('$RESULTS_FILE') as f:
    current = json.load(f)
with open('$BASELINE_FILE') as f:
    baseline = json.load(f)

cs = current['summary']
bs = baseline['summary']

# Display labels only — see the summary block. Keys stay untouched.
LABELS = {'retrieval': 'search-endpt (/api/search)'}

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

# Per-category regressions — the category set comes from the ONE registry
# (EVAL_CATEGORIES), so a newly registered prefix is watched here from the day
# it is registered instead of from the day someone remembers this list.
#
# A registered category with no test cases is REPORTED as empty rather than
# skipped in silence: the 15-pp threshold cannot say anything about 0 cases,
# and a category that says nothing must not read as a category that passed.
# It is informational — never a regression, so it does not move the exit code.
empty = []
for cat in json.loads(os.environ['EVAL_CATEGORIES']):
    cc = current['by_category'].get(cat, {'pass':0, 'total':0})
    bc = baseline['by_category'].get(cat, {'pass':0, 'total':0})
    if cc['total'] == 0:
        empty.append(f'{LABELS.get(cat, cat)} (baseline: {bc[\"total\"]} cases)')
        continue
    if bc['total'] == 0:
        continue
    c_rate = cc['pass'] / cc['total'] * 100
    b_rate = bc['pass'] / bc['total'] * 100
    if c_rate < b_rate - 15:
        regressions.append(f'{LABELS.get(cat, cat)}: {b_rate:.0f}% -> {c_rate:.0f}% (REGRESSION)')

# Latency regression (p95 > 2x baseline AND absolute increase > 500ms)
# Low absolute values (e.g. 89ms->202ms) are just jitter, not regressions
for t in current['by_type']:
    if t in baseline.get('by_type', {}):
        cp95 = current['by_type'][t].get('latency_p95', 0)
        bp95 = baseline['by_type'][t].get('latency_p95', 0)
        if bp95 > 0 and cp95 > bp95 * 2 and (cp95 - bp95) > 500:
            regressions.append(f'{LABELS.get(t, t)} P95 latency: {bp95}ms -> {cp95}ms (>2x REGRESSION)')

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

if empty:
    print('  EMPTY CATEGORIES (registered, no test cases — not evaluated):')
    for e in empty:
        print(f'    -- {e}')
    print()

# Corpus drift (M-W6): a pass-rate move is only attributable against the
# population it was measured on. Informational, never a regression.
cp, bp = current.get('population'), baseline.get('population')
if cp is not None or bp is not None:
    print(f'  Population: {bp} -> {cp} retrievable blocks')
    cbt = current.get('by_block_type') or {}
    bbt = baseline.get('by_block_type') or {}
    moved = [f'{n}: {bbt.get(n, \"absent\")} -> {cbt.get(n, \"absent\")}' for n in sorted(set(cbt) | set(bbt)) if cbt.get(n) != bbt.get(n)]
    if moved:
        print('    types changed: ' + ', '.join(moved))
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
  # BEFORE the cp — afterwards the baseline this run replaces is gone. Moving a
  # gate is allowed; moving it without a trace is not (design/05 §4.6 (3)).
  eval_baseline_diff "$RESULTS_FILE" "$BASELINE_FILE" "$BASELINE_DIFF_FILE"
  cp "$RESULTS_FILE" "$BASELINE_FILE"
  echo "Baseline updated: $BASELINE_FILE"
  echo "Baseline diff:    $BASELINE_DIFF_FILE"
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
