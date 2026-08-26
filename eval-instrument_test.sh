#!/usr/bin/env bash
#
# Gate for eval.sh's instrumentation (Welle M-W6).
#
# Usage: bash eval-instrument_test.sh
#        EVAL_GOLDEN_RESULTS=/path/to/eval-results-*.json bash eval-instrument_test.sh
#
# Sources eval.sh for EVAL_CATEGORIES / eval_census_json / eval_baseline_diff /
# eval_aggregate only — eval.sh returns before its .env preflight when it is
# sourced rather than executed, so this test needs neither .env nor a running
# server, fires no query and never runs an eval pass. Every fixture is
# synthetic; the one real file it touches is a past eval result, read-only, for
# the golden case.
#
# Three things are under test:
#   1. by_block_type / population in the results file — the corpus census the
#      run measured against (design/05 §4.6 (2)).
#   2. .eval-baseline.diff — the record --update-baseline leaves behind
#      (design/05 §4.6 (3)); the mandatory negative probe runs the gate against
#      a variant that only does the cp, the way the branch looked before M-W6.
#   3. Category 'derived' (prefix G) in the ONE registry, deliberately WITHOUT
#      test cases (design/05 §2.2c N-01) — reported as empty, never as passed.

set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=./eval.sh
source "${TEST_DIR}/eval.sh"

for fn in eval_census_json eval_baseline_diff eval_aggregate; do
  if ! declare -F "$fn" >/dev/null; then
    echo "[FATAL] $fn is not defined after sourcing eval.sh"
    exit 1
  fi
done
if [[ -z "${EVAL_CATEGORIES:-}" ]]; then
  echo "[FATAL] EVAL_CATEGORIES is not set after sourcing eval.sh"
  exit 1
fi

# Work dir next to the script, not under /tmp (world-readable). Removed on exit.
WORK="${EVAL_INSTRUMENT_WORKDIR:-${TEST_DIR}/.eval-instrument-work}"
rm -rf "$WORK"
mkdir -p "$WORK"
trap 'rm -rf "$WORK"' EXIT

T_PASS=0
T_FAIL=0
T_SKIP=0

pass() { T_PASS=$(( T_PASS + 1 )); printf '[OK]   %s\n' "$1"; }
failed() { T_FAIL=$(( T_FAIL + 1 )); printf '[FAIL] %-64s %s\n' "$1" "$2"; }
skip() { T_SKIP=$(( T_SKIP + 1 )); printf '[SKIP] %-64s %s\n' "$1" "$2"; }

# assert_eq <name> <expected> <actual>
assert_eq() {
  if [[ "$2" == "$3" ]]; then pass "$1"; else failed "$1" "expected [$2] got [$3]"; fi
}

# =====================================================================
# Fixtures
# =====================================================================

# A results array with two G-cases (one PASS, one FAIL) plus one of every
# established prefix, so the registry is exercised end to end.
cat > "$WORK/results-array-g.json" <<'JSON'
[
 {"id":"S01","type":"synthesis","verdict":"PASS","latency_ms":100,"desc":"s","confidence":"confident","expected_confidence":"confident","keyword_hits":2,"keyword_total":2},
 {"id":"B01","type":"synthesis","verdict":"PASS","latency_ms":110,"desc":"b","confidence":"confident","expected_confidence":"confident","keyword_hits":1,"keyword_total":1},
 {"id":"N01","type":"synthesis","verdict":"PASS","latency_ms":120,"desc":"n","confidence":"low_confidence","expected_confidence":"none","keyword_hits":0,"keyword_total":0},
 {"id":"K01","type":"synthesis","verdict":"PASS","latency_ms":130,"desc":"k","confidence":"confident","expected_confidence":"confident","keyword_hits":1,"keyword_total":1},
 {"id":"I01","type":"synthesis","verdict":"PASS","latency_ms":140,"desc":"i","confidence":"confident","expected_confidence":"confident","keyword_hits":1,"keyword_total":1},
 {"id":"M01","type":"synthesis","verdict":"PASS","latency_ms":150,"desc":"m","confidence":"confident","expected_confidence":"confident","keyword_hits":1,"keyword_total":1},
 {"id":"T01","type":"synthesis","verdict":"PASS","latency_ms":160,"desc":"t","confidence":"confident","expected_confidence":"confident","keyword_hits":1,"keyword_total":1},
 {"id":"R01","type":"retrieval","verdict":"PASS","latency_ms":30,"desc":"r","keyword_hits":1,"keyword_total":1},
 {"id":"G01","type":"synthesis","verdict":"PASS","latency_ms":200,"desc":"derived level","confidence":"confident","expected_confidence":"confident","keyword_hits":1,"keyword_total":1},
 {"id":"G02","type":"synthesis","verdict":"FAIL","latency_ms":300,"desc":"derived level","confidence":"low_confidence","expected_confidence":"confident","keyword_hits":0,"keyword_total":1}
]
JSON

# The same set without the two G-cases (the state the harness is in today).
python3 - "$WORK/results-array-g.json" "$WORK/results-array-nog.json" <<'PY'
import json, sys
rows = json.load(open(sys.argv[1]))
json.dump([r for r in rows if not r['id'].startswith('G')], open(sys.argv[2], 'w'))
PY

# A stats response with the drift section, shaped like the live one
# (go/internal/store/drift.go:23-46): two retrievable types, two excluded.
cat > "$WORK/stats-drift.json" <<'JSON'
{"action":"stats","success":true,
 "stats":{"total_blocks":7366,"total_categories":9,"db_size":"1 GB"},
 "drift":{"at":"2026-08-26T20:36:50.730126Z","retrievable_blocks":1388,
  "types":[
   {"type_name":"audit-trail","retrievable":true,"count":285,"null_embedding":0},
   {"type_name":"checkpoint","retrievable":false,"count":5955,"null_embedding":5352},
   {"type_name":"knowledge","retrievable":true,"count":982,"null_embedding":0},
   {"type_name":"reference","retrievable":true,"count":121,"null_embedding":0},
   {"type_name":"system-meta","retrievable":false,"count":23,"null_embedding":2}],
  "gold_ids":[]}}
JSON

# What a non-admin key gets back (handler/context_manage.go:804, verified live).
cat > "$WORK/stats-403.json" <<'JSON'
{"success":false,"error":"admin key required"}
JSON

# A census whose own sum contradicts the server's retrievable_blocks.
cat > "$WORK/stats-mismatch.json" <<'JSON'
{"success":true,"drift":{"at":"2026-08-26T20:36:50Z","retrievable_blocks":9999,
 "types":[{"type_name":"knowledge","retrievable":true,"count":10,"null_embedding":0}],"gold_ids":[]}}
JSON

jqget() { python3 -c "
import json, sys
d = json.load(open(sys.argv[1]))
for k in sys.argv[2].split('.'):
    if d is None:
        break
    d = d.get(k) if isinstance(d, dict) else None
print(json.dumps(d))
" "$1" "$2"; }

echo "================================================================="
echo "  eval.sh instrumentation — gate (Welle M-W6)"
echo "================================================================="
echo ""
echo "--- 1. Category registry (one registry, prefix G registered) ---"

REG_KEYS=$(python3 -c "import json,os;print(','.join(json.loads(os.environ['EVAL_CATEGORIES'])))")
assert_eq "registry order and membership" \
  "confident,bilingual,negative,keyword,imperative,multihop,temporal,retrieval,derived" "$REG_KEYS"
assert_eq "derived carries prefix G" "G" \
  "$(python3 -c "import json,os;print(json.loads(os.environ['EVAL_CATEGORIES'])['derived'])")"
assert_eq "every prefix is unique (no category shadows another)" "9" \
  "$(python3 -c "import json,os;v=list(json.loads(os.environ['EVAL_CATEGORIES']).values());print(len(set(v)))")"

echo ""
echo "--- 2. Block-type census ---"

CENSUS_OK=$(eval_census_json < "$WORK/stats-drift.json")
printf '%s' "$CENSUS_OK" > "$WORK/census-ok.json"
assert_eq "census: retrievable types only" \
  '{"audit-trail": 285, "knowledge": 982, "reference": 121}' \
  "$(jqget "$WORK/census-ok.json" by_block_type)"
assert_eq "census: population is the sum of the retrievable counts" "1388" \
  "$(jqget "$WORK/census-ok.json" population)"
assert_eq "census: source names the server clock" '"manage stats drift @ 2026-08-26T20:36:50.730126Z"' \
  "$(jqget "$WORK/census-ok.json" census_source)"

eval_census_json < "$WORK/stats-403.json" > "$WORK/census-403.json"
assert_eq "census: non-admin 403 leaves by_block_type null" "null" \
  "$(jqget "$WORK/census-403.json" by_block_type)"
assert_eq "census: 403 reason is carried, not swallowed" '"unavailable: admin key required"' \
  "$(jqget "$WORK/census-403.json" census_source)"

printf 'not json at all' | eval_census_json > "$WORK/census-garbage.json"
assert_eq "census: garbage response degrades to null, no crash" "null" \
  "$(jqget "$WORK/census-garbage.json" population)"

eval_census_json < "$WORK/stats-mismatch.json" > "$WORK/census-mismatch.json"
MISMATCH=$(jqget "$WORK/census-mismatch.json" census_source)
if [[ "$MISMATCH" == *"MISMATCH: server retrievable_blocks=9999"* ]]; then
  pass "census: sum disagreeing with the server is flagged, not smoothed over"
else
  failed "census: sum disagreeing with the server is flagged" "got $MISMATCH"
fi

echo ""
echo "--- 3. Aggregation: category G and the census fields ---"

printf '%s' "$CENSUS_OK" | eval_aggregate "$WORK/results-array-g.json" 7366 42 "2026-08-26T21:00:00Z" "$WORK/agg-g.json" >/dev/null
assert_eq "G-cases count into 'derived'" '{"pass": 1, "fail": 1, "total": 2}' \
  "$(jqget "$WORK/agg-g.json" by_category.derived)"
assert_eq "no case falls outside the registry any more" "10" \
  "$(python3 -c "
import json,sys
d = json.load(open('$WORK/agg-g.json'))
print(sum(c['total'] for c in d['by_category'].values()))")"
assert_eq "summary.total agrees with the category totals" "10" \
  "$(jqget "$WORK/agg-g.json" summary.total)"
assert_eq "aggregation carries by_block_type into the results file" \
  '{"audit-trail": 285, "knowledge": 982, "reference": 121}' \
  "$(jqget "$WORK/agg-g.json" by_block_type)"
assert_eq "aggregation carries population into the results file" "1388" \
  "$(jqget "$WORK/agg-g.json" population)"

# N-01: the registered category stays EMPTY until a live-capable derived level
# ships its cases. Empty must be visible (total 0), never absent.
printf '%s' "$CENSUS_OK" | eval_aggregate "$WORK/results-array-nog.json" 7366 42 "2026-08-26T21:00:00Z" "$WORK/agg-nog.json" >/dev/null
assert_eq "empty category is present with total 0, not missing" '{"pass": 0, "fail": 0, "total": 0}' \
  "$(jqget "$WORK/agg-nog.json" by_category.derived)"

# No census reachable: the run still produces a results file, and it says so.
printf '' | eval_aggregate "$WORK/results-array-nog.json" 7366 42 "2026-08-26T21:00:00Z" "$WORK/agg-nocensus.json" >/dev/null
assert_eq "no census: by_block_type null" "null" "$(jqget "$WORK/agg-nocensus.json" by_block_type)"
assert_eq "no census: the results file says why" '"unavailable: no census taken"' \
  "$(jqget "$WORK/agg-nocensus.json" census_source)"

echo ""
echo "--- 4. Baseline diff ---"

# Baseline = the good run; current = the same run with two cases turned FAIL and
# one turned PASS, on a corpus that grew by one type.
cp "$WORK/agg-g.json" "$WORK/base.json"
python3 - "$WORK/agg-g.json" "$WORK/worse.json" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
for r in d['results']:
    if r['id'] in ('S01', 'B01'):
        r['verdict'] = 'FAIL'
    if r['id'] == 'G02':
        r['verdict'] = 'PASS'
d['summary']['passed'] = 8
d['summary']['failed'] = 2
d['summary']['pass_rate'] = 80.0
d['by_category']['confident'] = {'pass': 0, 'fail': 1, 'total': 1}
d['by_category']['bilingual'] = {'pass': 0, 'fail': 1, 'total': 1}
d['by_category']['derived'] = {'pass': 2, 'fail': 0, 'total': 2}
d['by_block_type']['knowledge'] = 990
d['by_block_type']['insight'] = 12
d['population'] = 1408
d['timestamp'] = '2026-08-26T22:00:00Z'
json.dump(d, open(sys.argv[2], 'w'), indent=2)
PY

# The --update-baseline branch, with and without the diff step. The variant
# without it is the branch as it looked before M-W6.
variant_update_baseline() {
  local with_diff="$1" results="$2" baseline="$3" out="$4"
  if [[ "$with_diff" == "true" ]]; then
    eval_baseline_diff "$results" "$baseline" "$out" > "$WORK/digest.txt"
  fi
  cp "$results" "$baseline"
}

# gate_diff_visible <diff-file> — the gate itself: a baseline update must leave a
# diff file behind AND that file must name the cases that flipped.
gate_diff_visible() {
  [[ -s "$1" ]] || return 1
  local flips
  flips=$(python3 -c "
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except ValueError:
    print(-1); sys.exit(0)
print(len(d.get('flips') or []))" "$1")
  [[ "$flips" -gt 0 ]]
}

D_OK="$WORK/withdiff/.eval-baseline.diff"
mkdir -p "$WORK/withdiff"
cp "$WORK/base.json" "$WORK/withdiff/.eval-baseline.json"
variant_update_baseline true "$WORK/worse.json" "$WORK/withdiff/.eval-baseline.json" "$D_OK"

if gate_diff_visible "$D_OK"; then
  pass "worse run: baseline update leaves a diff with a non-empty flip list"
else
  failed "worse run: baseline update leaves a diff with a non-empty flip list" "gate red"
fi
assert_eq "flip list: two PASS->FAIL" "2" "$(jqget "$D_OK" flip_counts.pass_to_fail)"
assert_eq "flip list: one FAIL->PASS" "1" "$(jqget "$D_OK" flip_counts.fail_to_pass)"
assert_eq "flip list names the cases" '[{"id": "B01", "from": "PASS", "to": "FAIL"}, {"id": "G02", "from": "FAIL", "to": "PASS"}, {"id": "S01", "from": "PASS", "to": "FAIL"}]' \
  "$(jqget "$D_OK" flips)"
assert_eq "summary delta: old pass count" "9" "$(jqget "$D_OK" summary.from.passed)"
assert_eq "summary delta: new pass count" "8" "$(jqget "$D_OK" summary.to.passed)"
assert_eq "per-category pass/total for the empty category" '{"from": [1, 2], "to": [2, 2]}' \
  "$(jqget "$D_OK" by_category.derived)"
assert_eq "census delta: population" '{"from": 1388, "to": 1408}' "$(jqget "$D_OK" population)"
assert_eq "census delta: only the changed types" '{"insight": [null, 12], "knowledge": [982, 990]}' \
  "$(jqget "$D_OK" by_block_type.changed)"
assert_eq "both run timestamps are recorded" '{"from": "2026-08-26T21:00:00Z", "to": "2026-08-26T22:00:00Z"}' \
  "$(jqget "$D_OK" timestamps)"
if grep -q 'PASS->FAIL: B01, S01' "$WORK/digest.txt"; then
  pass "stdout digest names the cases that got worse"
else
  failed "stdout digest names the cases that got worse" "got: $(cat "$WORK/digest.txt")"
fi

# MANDATORY negative probe: the pre-M-W6 branch (cp only) must make the gate red.
D_NONE="$WORK/nodiff/.eval-baseline.diff"
mkdir -p "$WORK/nodiff"
cp "$WORK/base.json" "$WORK/nodiff/.eval-baseline.json"
variant_update_baseline false "$WORK/worse.json" "$WORK/nodiff/.eval-baseline.json" "$D_NONE"
if gate_diff_visible "$D_NONE"; then
  failed "NEGATIVE PROBE: variant without the diff step must fail the gate" "gate went green without a diff file"
else
  pass "NEGATIVE PROBE: variant without the diff step fails the gate (cp is traceless)"
fi
if [[ -f "$WORK/nodiff/.eval-baseline.json" ]]; then
  pass "NEGATIVE PROBE: the cp itself still happened (the variant is a real cp-only branch)"
else
  failed "NEGATIVE PROBE: the cp itself still happened" "baseline missing"
fi

# First baseline ever: one line, no pretend delta.
D_FIRST="$WORK/first/.eval-baseline.diff"
mkdir -p "$WORK/first"
variant_update_baseline true "$WORK/worse.json" "$WORK/first/.eval-baseline.json" "$D_FIRST"
assert_eq "no previous baseline: single line, not a fabricated delta" "no previous baseline" \
  "$(cat "$D_FIRST")"

echo ""
echo "--- 5. Golden: a real past result through the new aggregation ---"

GOLDEN="${EVAL_GOLDEN_RESULTS:-}"
if [[ -z "$GOLDEN" ]]; then
  if [[ -f "${TEST_DIR}/.eval-results/eval-results-1787770683.json" ]]; then
    GOLDEN="${TEST_DIR}/.eval-results/eval-results-1787770683.json"
  else
    GOLDEN=$(find "${TEST_DIR}/.eval-results" -maxdepth 1 -name 'eval-results-*.json' 2>/dev/null | sort | tail -1)
  fi
fi

if [[ -z "$GOLDEN" || ! -f "$GOLDEN" ]]; then
  skip "golden: past result reproduces its own summary/by_category" \
       "no eval-results-*.json found (set EVAL_GOLDEN_RESULTS)"
else
  python3 -c "
import json, sys
json.dump(json.load(open(sys.argv[1]))['results'], open(sys.argv[2], 'w'))" "$GOLDEN" "$WORK/golden-array.json"
  G_TB=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['total_blocks'])" "$GOLDEN")
  G_EL=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['elapsed_seconds'])" "$GOLDEN")
  G_TS=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1]))['timestamp'])" "$GOLDEN")
  printf '' | eval_aggregate "$WORK/golden-array.json" "$G_TB" "$G_EL" "$G_TS" "$WORK/golden-out.json" >/dev/null
  GOLD_DIFF=$(python3 -c "
import json, sys
old = json.load(open(sys.argv[1]))
new = json.load(open(sys.argv[2]))
bad = []
for key in ('summary', 'by_type', 'results', 'timestamp', 'total_blocks', 'elapsed_seconds'):
    if old.get(key) != new.get(key):
        bad.append(key)
# by_category: the established categories must be identical; 'derived' is the
# one additive key and must be empty on a corpus that has no G-cases.
for cat, val in old['by_category'].items():
    if new['by_category'].get(cat) != val:
        bad.append('by_category.' + cat)
extra = set(new['by_category']) - set(old['by_category'])
if extra != {'derived'}:
    bad.append('by_category extra keys: %s' % sorted(extra))
if new['by_category'].get('derived') != {'pass': 0, 'fail': 0, 'total': 0}:
    bad.append('derived not empty')
print(','.join(bad) if bad else 'identical')" "$GOLDEN" "$WORK/golden-out.json")
  assert_eq "golden ($(basename "$GOLDEN")): summary/by_type/by_category/results unchanged" \
    "identical" "$GOLD_DIFF"
fi

echo ""
echo "-----------------------------------------------------------------"
printf '  %d passed, %d failed, %d skipped (%d cases)\n' \
  "$T_PASS" "$T_FAIL" "$T_SKIP" "$(( T_PASS + T_FAIL + T_SKIP ))"
echo "-----------------------------------------------------------------"

if (( T_FAIL > 0 )); then
  exit 1
fi
exit 0
