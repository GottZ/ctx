#!/usr/bin/env bash
#
# Temporal Retrieval Quality Evaluation
# Tests whether cycle-proportional temporal search windows improve retrieval.
#
# Hypothesis: Expanding the temporal search window proportional to the cycle
# length (weekday ±7d, month ±30d) improves recall without hurting precision.
#
# Usage: bash eval-temporal.sh              — full eval (~8 min, 10 blocks, api/query + RRF)
#        bash eval-temporal.sh --search-only — fast mode (~1 min, 20 blocks, api/search FTS only)
#        bash eval-temporal.sh --dry-run     — show test cases without execution
#
# NOTE: --search-only uses api/search which is FTS-only (no embeddings).
# Temporal queries with shifted dates will score poorly there by design.
# The full mode (api/query) is the meaningful test because it uses the
# complete RRF pipeline: semantic + DE-FTS + EN-FTS + trigram + temporal expansion.
#
# Metrics: Recall@5, MRR, Precision@5 (at each temporal offset category)
#
# ctx — Your AI's save game. By GottZ (github.com/GottZ/ctx/graphs/contributors)

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${SCRIPT_DIR}/.env"

if [[ ! -f "$ENV_FILE" ]]; then
    echo "[FATAL] .env not found at $ENV_FILE"
    exit 1
fi
set -a; source "$ENV_FILE"; set +a

WEBHOOK="${WEBHOOK_BASE_URL:-https://localhost}"
KEY="${CONTEXT_API_KEY_PRIVATE:?CONTEXT_API_KEY_PRIVATE not set in .env}"

SEARCH_ONLY=false
DRY_RUN=false
for arg in "$@"; do
  case "$arg" in
    --search-only) SEARCH_ONLY=true ;;
    --dry-run) DRY_RUN=true ;;
  esac
done

# =====================================================================
# Helpers
# =====================================================================

api() {
  local timeout="${3:-120}"
  curl -s --max-time "$timeout" -X POST "$1" \
    -H "Content-Type: application/json" \
    -H "X-Context-Key: $KEY" \
    -d "$2" 2>/dev/null
}

# =====================================================================
# Phase 1: Extract Temporal Ground Truth from the Live Store
# =====================================================================
# Query the DB for blocks with explicit dates, build test cases dynamically.
# This avoids hardcoding block IDs that may change.

DB_CMD="docker exec -e PGPASSWORD=${CONTEXT_DB_PASSWORD:?CONTEXT_DB_PASSWORD not set in .env} n8n-db-1 psql -U ${CONTEXT_DB_USER:-context_user} -d ${CONTEXT_DB:-context_store} -t -A"

echo "================================================================="
echo "  Temporal Retrieval Evaluation"
echo "  $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "================================================================="
echo ""

# Block limit: 8 for full mode (LLM calls ~5-10s each), 20 for search-only.
if $SEARCH_ONLY; then
    BLOCK_LIMIT=20
else
    BLOCK_LIMIT=8
fi

# Get blocks with clear single-date references and enough content to query about.
# Format: id|title|category|date|content_start
TEMPORAL_BLOCKS=$($DB_CMD -c "
SELECT cb.id, cb.title, cb.category,
       (SELECT m[1]::date FROM regexp_matches(cb.content, '(\d{4}-\d{2}-\d{2})', 'g') m LIMIT 1) as first_date,
       substring(replace(replace(cb.content, E'\n', ' '), '  ', ' '), 1, 200) as content_start
FROM context_blocks cb
WHERE NOT cb.is_archived
  AND cb.content ~ '\d{4}-\d{2}-\d{2}'
  AND cb.category IN ('learnings', 'decisions', 'reference', 'infrastructure', 'projects')
  AND length(cb.content) > 200
  AND (SELECT count(*) FROM regexp_matches(cb.content, '\d{4}-\d{2}-\d{2}', 'g')) <= 3
ORDER BY cb.created_at DESC
LIMIT $BLOCK_LIMIT;
" 2>/dev/null)

if [[ -z "$TEMPORAL_BLOCKS" ]]; then
    echo "[FATAL] No temporal blocks found in the store"
    exit 1
fi

BLOCK_COUNT=$(echo "$TEMPORAL_BLOCKS" | wc -l)
echo "Phase 1: Found $BLOCK_COUNT blocks with clear date references"
echo ""

# =====================================================================
# Phase 2: Generate Test Cases
# =====================================================================
# For each block, create queries at different temporal offsets.
# The query uses the block's topic but shifts the date reference.
#
# Offset categories:
#   exact    — correct date (±0d)
#   drift_1d — ±1 day (minor calendar confusion)
#   drift_7d — ±7 days (weekday cycle confusion — "last Monday" vs "this Monday")
#   drift_30d — ±30 days (month confusion — "in March" vs "in February")
#
# For each offset, we create a query that mentions the shifted date
# and check if the original block still appears in results.

RESULTS_FILE="/tmp/eval-temporal-$(date +%s).csv"
echo "test_id,block_id,block_title,block_date,query_date,offset_days,offset_category,endpoint,target_found,target_rank,result_count,latency_ms" > "$RESULTS_FILE"

# =====================================================================
# Phase 2b: Query Templates
# =====================================================================
# German and English templates that incorporate a date reference.
# The %DATE% placeholder is replaced with the shifted date.
# The %TOPIC% placeholder gets a short topic extracted from the title.

generate_queries() {
    local title="$1"
    local date_str="$2"
    local topic

    # Extract a searchable topic from the title (first 3-4 meaningful words).
    topic=$(echo "$title" | sed 's/ — /: /g; s/ - /: /g' | cut -d: -f1 | head -c 60)

    # Determine weekday name for the date (German).
    local weekday_de
    weekday_de=$(python3 -c "
import datetime
d = datetime.date.fromisoformat('$date_str')
days_de = ['Montag','Dienstag','Mittwoch','Donnerstag','Freitag','Samstag','Sonntag']
print(days_de[d.weekday()])
" 2>/dev/null)

    # Determine month name (German).
    local month_de
    month_de=$(python3 -c "
import datetime
d = datetime.date.fromisoformat('$date_str')
months = ['Januar','Februar','März','April','Mai','Juni','Juli','August','September','Oktober','November','Dezember']
print(months[d.month-1])
" 2>/dev/null)

    # Return multiple query variants, one per line: format "lang|query"
    cat <<QUERIES
de|Was wurde am $date_str bezüglich $topic entschieden?
en|What happened with $topic on $date_str?
de|$topic vom $weekday_de $date_str
en|$topic changes in $month_de
QUERIES
}

# Compute offset dates.
compute_offset_date() {
    local base_date="$1"
    local offset_days="$2"
    python3 -c "
import datetime
d = datetime.date.fromisoformat('$base_date')
delta = datetime.timedelta(days=$offset_days)
print((d + delta).isoformat())
" 2>/dev/null
}

# =====================================================================
# Phase 3: Execute Test Matrix
# =====================================================================
# For each block × offset × query variant, call the API and record results.

# Offset selection: full mode uses key offsets only (0, ±7, ±30) for speed.
# search-only mode adds ±1, ±14 (cheap FTS calls).
if $SEARCH_ONLY; then
    OFFSETS="0 1 -1 7 -7 14 -14 30 -30"
else
    OFFSETS="0 7 -7 30 -30"
fi
TOTAL_TESTS=0
TOTAL_FOUND=0
TEST_NUM=0

echo "Phase 2: Generating test matrix..."

# Pre-compute all test cases into an array for progress tracking.
declare -a TEST_CASES=()

while IFS='|' read -r block_id block_title block_category block_date content_start; do
    [[ -z "$block_id" ]] && continue

    for offset in $OFFSETS; do
        query_date=$(compute_offset_date "$block_date" "$offset")

        # Offset category label
        abs_offset=${offset#-}
        if [[ "$abs_offset" == "0" ]]; then
            offset_cat="exact"
        elif [[ "$abs_offset" == "1" ]]; then
            offset_cat="drift_1d"
        elif [[ "$abs_offset" == "7" ]]; then
            offset_cat="drift_7d"
        elif [[ "$abs_offset" == "14" ]]; then
            offset_cat="drift_14d"
        elif [[ "$abs_offset" == "30" ]]; then
            offset_cat="drift_30d"
        else
            offset_cat="drift_${abs_offset}d"
        fi

        # Use only the ISO-date query variant (most controlled).
        # For exact: also test the weekday/month variants.
        if [[ "$offset" == "0" ]]; then
            # Exact match: test with ISO date reference
            TEST_CASES+=("$block_id|$block_title|$block_date|$query_date|$offset|$offset_cat|de|Was wurde am $query_date bezüglich $block_title entschieden?")
        else
            # Offset: test with ISO date (shifted)
            TEST_CASES+=("$block_id|$block_title|$block_date|$query_date|$offset|$offset_cat|de|Was passierte am $query_date zu $block_title?")
            # Also test with just the topic (no date) — measures pure semantic recall
            if [[ "$abs_offset" == "7" ]] || [[ "$abs_offset" == "30" ]]; then
                TEST_CASES+=("$block_id|$block_title|$block_date|$query_date|$offset|${offset_cat}_semantic|en|$block_title")
            fi
        fi
    done
done <<< "$TEMPORAL_BLOCKS"

TOTAL_PLANNED=${#TEST_CASES[@]}
echo "Phase 2: $TOTAL_PLANNED test cases generated from $BLOCK_COUNT blocks"
echo ""

if $DRY_RUN; then
    echo "--- DRY RUN: Test Cases ---"
    echo ""
    printf "%-36s %-12s %-12s %-10s %s\n" "BLOCK_ID" "BLOCK_DATE" "QUERY_DATE" "OFFSET" "QUERY"
    echo "$(printf '%.0s-' {1..120})"
    for tc in "${TEST_CASES[@]}"; do
        IFS='|' read -r bid btitle bdate qdate offs offcat lang query <<< "$tc"
        printf "%-36s %-12s %-12s %-10s %s\n" "${bid:0:36}" "$bdate" "$qdate" "$offs" "${query:0:60}"
    done
    echo ""
    echo "Total: $TOTAL_PLANNED test cases"
    exit 0
fi

# =====================================================================
# Phase 3: Execute
# =====================================================================

START_TIME=$(date +%s)

echo "Phase 3: Executing $TOTAL_PLANNED tests..."
echo ""

# Choose endpoint based on mode
if $SEARCH_ONLY; then
    ENDPOINT="api/search"
    echo "Mode: search-only (api/search, no LLM, ~2-4s per test)"
else
    ENDPOINT="api/query"
    echo "Mode: full (api/query, with LLM synthesis, ~5-10s per test)"
fi
echo ""

printf "%-6s %-8s %-10s %-8s %-4s %-6s %s\n" "NUM" "OFFSET" "CAT" "FOUND" "RANK" "MS" "TITLE"
echo "$(printf '%.0s-' {1..90})"

for tc in "${TEST_CASES[@]}"; do
    IFS='|' read -r block_id block_title block_date query_date offset offset_cat lang query <<< "$tc"
    TEST_NUM=$((TEST_NUM + 1))
    TOTAL_TESTS=$((TOTAL_TESTS + 1))

    # Escape query for JSON
    escaped_query=$(printf '%s' "$query" | python3 -c "import sys,json; print(json.dumps(sys.stdin.read()))")

    t_start=$(date +%s%3N)

    if $SEARCH_ONLY; then
        resp=$(api "${WEBHOOK}/${ENDPOINT}" "{\"query\":$escaped_query,\"limit\":10}" 30)

        # Parse search results — check if target block_id appears
        result_info=$(echo "$resp" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    results = d.get('results', [])
    count = len(results)
    found = False
    rank = 0
    for i, r in enumerate(results):
        if r.get('id') == '$block_id':
            found = True
            rank = i + 1
            break
    print(f'{found}|{rank}|{count}')
except:
    print('False|0|0')
" 2>/dev/null)
    else
        resp=$(api "${WEBHOOK}/${ENDPOINT}" "{\"query\":$escaped_query,\"limit\":10}" 120)

        # Parse agent results — check sources for target block_id
        result_info=$(echo "$resp" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    sources = d.get('sources', [])
    count = len(sources)
    found = False
    rank = 0
    for i, s in enumerate(sources):
        if s.get('id') == '$block_id':
            found = True
            rank = i + 1
            break
    print(f'{found}|{rank}|{count}')
except:
    print('False|0|0')
" 2>/dev/null)
    fi

    t_end=$(date +%s%3N)
    latency_ms=$(( t_end - t_start ))

    IFS='|' read -r found rank result_count <<< "$result_info"

    if [[ "$found" == "True" ]]; then
        TOTAL_FOUND=$((TOTAL_FOUND + 1))
        found_display="YES"
    else
        found_display="no"
    fi

    # Output progress line
    printf "%-6s %-8s %-10s %-8s %-4s %-6s %s\n" \
        "$TEST_NUM/$TOTAL_PLANNED" "$offset" "$offset_cat" "$found_display" "$rank" "${latency_ms}" "${block_title:0:40}"

    # Write CSV
    # Escape title for CSV (replace commas and quotes)
    csv_title=$(echo "$block_title" | tr ',' ';' | tr '"' "'")
    echo "$TEST_NUM,$block_id,$csv_title,$block_date,$query_date,$offset,$offset_cat,$ENDPOINT,$found,$rank,$result_count,$latency_ms" >> "$RESULTS_FILE"
done

END_TIME=$(date +%s)
ELAPSED=$(( END_TIME - START_TIME ))

# =====================================================================
# Phase 4: Analysis
# =====================================================================

echo ""
echo "================================================================="
echo "  ANALYSIS"
echo "================================================================="
echo ""

python3 - "$RESULTS_FILE" <<'PYEOF'
import csv, sys
from collections import defaultdict

results_file = sys.argv[1]

rows = []
with open(results_file) as f:
    reader = csv.DictReader(f)
    for row in reader:
        row['target_found'] = row['target_found'] == 'True'
        row['target_rank'] = int(row['target_rank'])
        row['result_count'] = int(row['result_count'])
        row['latency_ms'] = int(row['latency_ms'])
        row['offset_days'] = int(row['offset_days'])
        rows.append(row)

if not rows:
    print("  No results to analyze.")
    sys.exit(0)

# --- Metrics per offset category ---
cats = defaultdict(lambda: {'total': 0, 'found': 0, 'ranks': [], 'latencies': []})

for r in rows:
    cat = r['offset_category']
    cats[cat]['total'] += 1
    if r['target_found']:
        cats[cat]['found'] += 1
        cats[cat]['ranks'].append(r['target_rank'])
    cats[cat]['latencies'].append(r['latency_ms'])

# Sort by absolute offset value for display
cat_order = ['exact', 'drift_1d', 'drift_7d', 'drift_7d_semantic', 'drift_14d', 'drift_30d', 'drift_30d_semantic']
cat_order = [c for c in cat_order if c in cats]
# Add any remaining
for c in sorted(cats.keys()):
    if c not in cat_order:
        cat_order.append(c)

print("  Offset Category     Recall@10   MRR       Hit/Total   Avg Latency")
print("  " + "-" * 70)

for cat in cat_order:
    d = cats[cat]
    total = d['total']
    found = d['found']
    recall = found / total if total > 0 else 0

    # MRR: Mean Reciprocal Rank (1/rank for found, 0 for not found)
    mrr_sum = sum(1.0 / r for r in d['ranks']) if d['ranks'] else 0
    mrr = mrr_sum / total if total > 0 else 0

    avg_lat = sum(d['latencies']) // len(d['latencies']) if d['latencies'] else 0

    print(f"  {cat:22s} {recall:6.1%}      {mrr:5.3f}     {found:3d}/{total:<3d}     {avg_lat:5d}ms")

print()

# --- Aggregate summary ---
total_tests = len(rows)
total_found = sum(1 for r in rows if r['target_found'])
overall_recall = total_found / total_tests if total_tests > 0 else 0
all_ranks = [r['target_rank'] for r in rows if r['target_found']]
overall_mrr = sum(1.0/r for r in all_ranks) / total_tests if total_tests > 0 else 0

print(f"  Overall: {total_found}/{total_tests} found ({overall_recall:.1%} recall), MRR={overall_mrr:.3f}")
print()

# --- Recall degradation curve ---
print("  Recall Degradation Curve (by absolute offset):")
print("  " + "-" * 50)
offsets = defaultdict(lambda: {'total': 0, 'found': 0})
for r in rows:
    if '_semantic' in r['offset_category']:
        continue  # Skip semantic-only tests for this curve
    abs_off = abs(r['offset_days'])
    offsets[abs_off]['total'] += 1
    if r['target_found']:
        offsets[abs_off]['found'] += 1

for off in sorted(offsets.keys()):
    d = offsets[off]
    recall = d['found'] / d['total'] if d['total'] > 0 else 0
    bar = "#" * int(recall * 40)
    print(f"  ±{off:2d}d: {recall:5.1%} ({d['found']:2d}/{d['total']:2d}) {bar}")

print()

# --- Rank distribution for found blocks ---
if all_ranks:
    print("  Rank Distribution (found blocks):")
    print("  " + "-" * 40)
    rank_dist = defaultdict(int)
    for r in all_ranks:
        rank_dist[r] += 1
    for rank in sorted(rank_dist.keys()):
        count = rank_dist[rank]
        pct = count / len(all_ranks) * 100
        bar = "#" * int(pct / 2)
        print(f"  Rank {rank:2d}: {count:3d} ({pct:4.1f}%) {bar}")
    print()

# --- Hypothesis Evaluation ---
print("  =============================================")
print("  HYPOTHESIS EVALUATION")
print("  =============================================")
print()

exact = cats.get('exact', {'total': 0, 'found': 0})
exact_recall = exact['found'] / exact['total'] if exact['total'] > 0 else 0

drift_7d = cats.get('drift_7d', {'total': 0, 'found': 0})
drift_7d_recall = drift_7d['found'] / drift_7d['total'] if drift_7d['total'] > 0 else 0

drift_30d = cats.get('drift_30d', {'total': 0, 'found': 0})
drift_30d_recall = drift_30d['found'] / drift_30d['total'] if drift_30d['total'] > 0 else 0

print(f"  Baseline (exact date): {exact_recall:.1%} recall ({exact['found']}/{exact['total']})")
print(f"  ±7d (weekday cycle):   {drift_7d_recall:.1%} recall ({drift_7d['found']}/{drift_7d['total']})")
print(f"  ±30d (month cycle):    {drift_30d_recall:.1%} recall ({drift_30d['found']}/{drift_30d['total']})")
print()

# Semantic-only comparison (measures how well the system retrieves by topic alone)
sem_7d = cats.get('drift_7d_semantic', {'total': 0, 'found': 0})
sem_30d = cats.get('drift_30d_semantic', {'total': 0, 'found': 0})
if sem_7d['total'] > 0 or sem_30d['total'] > 0:
    print("  Semantic-only (no date in query, topic-match only):")
    if sem_7d['total'] > 0:
        r = sem_7d['found'] / sem_7d['total']
        print(f"    7d offset blocks:  {r:.1%} recall ({sem_7d['found']}/{sem_7d['total']})")
    if sem_30d['total'] > 0:
        r = sem_30d['found'] / sem_30d['total']
        print(f"    30d offset blocks: {r:.1%} recall ({sem_30d['found']}/{sem_30d['total']})")
    print()

if exact_recall > 0 and drift_7d_recall > 0:
    drop_7d = (exact_recall - drift_7d_recall) / exact_recall * 100
    print(f"  Recall drop at ±7d:  {drop_7d:+.1f}%")
else:
    print("  Recall drop at ±7d:  insufficient data")

if exact_recall > 0 and drift_30d_recall > 0:
    drop_30d = (exact_recall - drift_30d_recall) / exact_recall * 100
    print(f"  Recall drop at ±30d: {drop_30d:+.1f}%")
else:
    print("  Recall drop at ±30d: insufficient data")

print()

# Decision guidance
if drift_7d_recall < exact_recall * 0.8:
    print("  SIGNAL: ±7d recall drops >20% — weekday-cycle window expansion likely beneficial.")
    print("  ACTION: Implement ±7d FTS expansion for weekday-referenced temporal queries.")
else:
    print("  SIGNAL: ±7d recall stable — weekday cycle confusion is NOT a retrieval bottleneck.")
    print("  ACTION: Do NOT implement weekday-cycle expansion (would add noise without gain).")

print()

if drift_30d_recall < exact_recall * 0.7:
    print("  SIGNAL: ±30d recall drops >30% — month-cycle window expansion likely beneficial.")
    print("  ACTION: Implement ±30d FTS expansion for month-referenced temporal queries.")
else:
    print("  SIGNAL: ±30d recall stable — month cycle confusion is NOT a retrieval bottleneck.")
    print("  ACTION: Do NOT implement month-cycle expansion (would hurt precision).")

print()
PYEOF

echo "================================================================="
echo "  Completed in ${ELAPSED}s"
echo "  Results: $RESULTS_FILE"
echo "================================================================="
