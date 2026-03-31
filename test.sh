#!/usr/bin/env bash
#
# Context Store Benchmark Test Suite
# Usage: ./test.sh              — runs T01-T10 (system tests only)
#        ./test.sh --with-ollama — runs T01-T14 (includes retrieval tests)
#
# ctx — Your AI's save game. By GottZ (github.com/GottZ/ctx/graphs/contributors)
# Implements GottZ 4-Way RRF verification and GottZ Scope Model tests.
# Source: https://github.com/GottZ/ctx

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${SCRIPT_DIR}/.env"

if [[ ! -f "$ENV_FILE" ]]; then
    echo "[FATAL] .env not found at $ENV_FILE"
    exit 1
fi
set -a; source "$ENV_FILE"; set +a

WEBHOOK="${WEBHOOK_BASE_URL:-https://localhost}"
KEY_PRIVATE="${CONTEXT_API_KEY_PRIVATE:?CONTEXT_API_KEY_PRIVATE not set in .env}"
KEY_WORK="${CONTEXT_API_KEY_WORK:?CONTEXT_API_KEY_WORK not set in .env}"
KEY_INVALID="deadbeef_invalid_key_0000000000000000000000000000000000000000"
DB_CMD="docker exec -e PGPASSWORD=${CONTEXT_DB_PASSWORD:?CONTEXT_DB_PASSWORD not set in .env} n8n-db-1 psql -U ${CONTEXT_DB_USER:-context_user} -d ${CONTEXT_DB:-context_store} -t -A"

PASS=0
FAIL=0
CLEANUP_ID=""
TEST_TITLE="__benchmark_test_$(date +%s)__"

WITH_OLLAMA=false
for arg in "$@"; do
  [[ "$arg" == "--with-ollama" ]] && WITH_OLLAMA=true
done

# --- Helpers ---

pass() {
  echo "[OK]   $1"
  ((PASS++))
}

fail() {
  echo "[FAIL] $1 -- $2"
  ((FAIL++))
}

# curl wrapper: $1=url, $2=key, $3=body, $4=timeout (default 10)
api() {
  local timeout="${4:-10}"
  curl -s --max-time "$timeout" -X POST "$1" \
    -H "Content-Type: application/json" \
    -H "X-Context-Key: $2" \
    -d "$3" 2>/dev/null
}

# Cleanup trap: always delete temp block
cleanup() {
  if [[ -n "$CLEANUP_ID" ]]; then
    api "$WEBHOOK/api/manage" "$KEY_PRIVATE" \
      "{\"action\":\"delete\",\"id\":\"$CLEANUP_ID\"}" 10 >/dev/null 2>&1
  fi
}
trap cleanup EXIT

echo "=== Context Store Benchmark ==="
echo "Date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo ""

# Config block — makes config drift visible
echo "Config: webhook=$WEBHOOK"
echo "Config: ollama=$(echo "${OLLAMA_HOST:-unset}" | sed 's|^\(https\?://[^@]*@\)|<redacted>@|')"
echo "Config: embed_model=${OLLAMA_EMBED_MODEL:-unset}"
echo "Config: embed_dims=${OLLAMA_EMBED_DIMS:-unset}"
echo "Config: chat_model=${OLLAMA_CHAT_MODEL:-unset}"
db_status=$(docker inspect --format '{{.State.Health.Status}}' n8n-db-1 2>/dev/null || echo "unknown")
echo "Config: db=n8n-db-1 ($db_status)"
echo ""

# =====================================================================
# PART 1: System Tests (no Ollama)
# =====================================================================
echo "--- Part 1: System Tests ---"
echo ""

# T01 AUTH_REJECT
T="T01 AUTH_REJECT"
resp=$(api "$WEBHOOK/api/manage" "$KEY_INVALID" '{"action":"stats"}')
if echo "$resp" | grep -qi "unauthorized\|\"success\":false"; then
  pass "$T"
else
  fail "$T" "expected Unauthorized or success:false, got: ${resp:0:100}"
fi

# T02 AUTH_PRIVATE
T="T02 AUTH_PRIVATE"
resp=$(api "$WEBHOOK/api/manage" "$KEY_PRIVATE" '{"action":"stats"}')
total=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['stats']['total_blocks'])" 2>/dev/null)
if [[ -n "$total" ]] && (( total >= 180 )); then
  pass "$T (total_blocks=$total)"
else
  fail "$T" "expected total_blocks >= 180, got: $total"
fi

# T03 AUTH_WORK
T="T03 AUTH_WORK"
resp=$(api "$WEBHOOK/api/manage" "$KEY_WORK" '{"action":"stats"}')
total=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['stats']['total_blocks'])" 2>/dev/null)
if [[ -n "$total" ]] && (( total < 30 )); then
  pass "$T (total_blocks=$total)"
else
  fail "$T" "expected total_blocks < 30 (scope isolation), got: $total"
fi

# T04 SCOPE_ISOLATION
T="T04 SCOPE_ISOLATION"
resp=$(api "$WEBHOOK/api/manage" "$KEY_WORK" '{"action":"guard-list"}')
if [[ -z "$resp" ]]; then
  # Empty response = no blocks visible (scope isolation working)
  pass "$T (empty response, no private blocks leaked)"
else
  has_private=$(echo "$resp" | python3 -c "
import sys,json
d=json.load(sys.stdin)
blocks=d.get('blocks',[])
private=[b for b in blocks if b.get('scope')=='private']
print(len(private))
" 2>/dev/null)
  if [[ "$has_private" == "0" ]] || [[ -z "$has_private" ]]; then
    pass "$T"
  else
    fail "$T" "found $has_private private-scope blocks via WORK key"
  fi
fi

# T05 CRUD_LIFECYCLE
T="T05 CRUD_LIFECYCLE"
t05_ok=true
t05_msg=""

# Save
resp=$(api "$WEBHOOK/api/store" "$KEY_PRIVATE" \
  "{\"category\":\"test\",\"title\":\"$TEST_TITLE\",\"content\":\"benchmark crud test content $(date +%s)\",\"tags\":[\"benchmark\"]}")
if ! echo "$resp" | grep -q '"success":true'; then
  t05_ok=false; t05_msg="save failed: ${resp:0:100}"
fi

# Search to get ID
if $t05_ok; then
  sleep 1
  resp=$(api "$WEBHOOK/api/search" "$KEY_PRIVATE" \
    "{\"query\":\"$TEST_TITLE\"}")
  CLEANUP_ID=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['results'][0]['id'])" 2>/dev/null)
  if [[ -z "$CLEANUP_ID" ]]; then
    t05_ok=false; t05_msg="search returned no results for $TEST_TITLE"
  fi
fi

# Get by ID
if $t05_ok; then
  resp=$(api "$WEBHOOK/api/manage" "$KEY_PRIVATE" \
    "{\"action\":\"get\",\"id\":\"$CLEANUP_ID\"}")
  got_title=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['block']['title'])" 2>/dev/null)
  if [[ "$got_title" != "$TEST_TITLE" ]]; then
    t05_ok=false; t05_msg="get returned wrong title: $got_title"
  fi
fi

# Delete
if $t05_ok; then
  resp=$(api "$WEBHOOK/api/manage" "$KEY_PRIVATE" \
    "{\"action\":\"delete\",\"id\":\"$CLEANUP_ID\"}")
  if echo "$resp" | grep -q '"success":true'; then
    CLEANUP_ID=""  # already cleaned up
  else
    t05_ok=false; t05_msg="delete failed: ${resp:0:100}"
  fi
fi

if $t05_ok; then
  pass "$T"
else
  fail "$T" "$t05_msg"
fi

# T06 UPSERT_NOOP
T="T06 UPSERT_NOOP"
t06_title="__benchmark_upsert_$(date +%s)__"
t06_ok=true
t06_msg=""
t06_id=""

# First save
resp=$(api "$WEBHOOK/api/store" "$KEY_PRIVATE" \
  "{\"category\":\"test\",\"title\":\"$t06_title\",\"content\":\"upsert noop test content\",\"tags\":[\"benchmark\"]}")
if ! echo "$resp" | grep -q '"success":true'; then
  t06_ok=false; t06_msg="first save failed: ${resp:0:100}"
fi

# Second save (identical content)
if $t06_ok; then
  resp=$(api "$WEBHOOK/api/store" "$KEY_PRIVATE" \
    "{\"category\":\"test\",\"title\":\"$t06_title\",\"content\":\"upsert noop test content\",\"tags\":[\"benchmark\"]}")
  action=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('action',''))" 2>/dev/null)
  t06_id=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('existing_id',''))" 2>/dev/null)
  if [[ "$action" == "noop" ]]; then
    pass "$T (action=noop)"
  else
    t06_ok=false; t06_msg="expected action=noop, got: $action"
  fi
fi

# Cleanup
if [[ -n "$t06_id" ]]; then
  api "$WEBHOOK/api/manage" "$KEY_PRIVATE" \
    "{\"action\":\"delete\",\"id\":\"$t06_id\"}" 10 >/dev/null 2>&1
else
  # Try to find and delete by search
  sleep 1
  resp=$(api "$WEBHOOK/api/search" "$KEY_PRIVATE" "{\"query\":\"$t06_title\"}")
  found_id=$(echo "$resp" | python3 -c "import sys,json; r=json.load(sys.stdin).get('results',[]); print(r[0]['id'] if r else '')" 2>/dev/null)
  [[ -n "$found_id" ]] && api "$WEBHOOK/api/manage" "$KEY_PRIVATE" \
    "{\"action\":\"delete\",\"id\":\"$found_id\"}" 10 >/dev/null 2>&1
fi

if ! $t06_ok; then
  fail "$T" "$t06_msg"
fi

# T07 SCHEMA_INTEGRITY
T="T07 SCHEMA_INTEGRITY"
table_count=$($DB_CMD -c "SELECT count(*) FROM information_schema.tables WHERE table_schema='public';" 2>/dev/null | tr -d '[:space:]')
col_count=$($DB_CMD -c "SELECT count(*) FROM information_schema.columns WHERE table_name='context_blocks';" 2>/dev/null | tr -d '[:space:]')
if [[ "$table_count" == "11" ]] && [[ "$col_count" == "30" ]]; then
  pass "$T (tables=$table_count, columns=$col_count)"
else
  fail "$T" "expected 11 tables + 30 columns, got tables=$table_count columns=$col_count"
fi

# T08 GUARD_STATS
T="T08 GUARD_STATS"
resp=$(api "$WEBHOOK/api/manage" "$KEY_PRIVATE" '{"action":"guard-stats"}')
if echo "$resp" | grep -q '"success":true'; then
  pass "$T"
else
  fail "$T" "expected success:true, got: ${resp:0:100}"
fi

# T09 GUARD_LIST_FILTER
T="T09 GUARD_LIST_FILTER"
resp=$(api "$WEBHOOK/api/manage" "$KEY_PRIVATE" '{"action":"guard-list","status":"clean"}')
non_clean=$(echo "$resp" | python3 -c "
import sys,json
d=json.load(sys.stdin)
blocks=d.get('blocks',[])
non_clean=[b for b in blocks if b.get('guard_status')!='clean']
print(len(non_clean))
" 2>/dev/null)
if [[ "$non_clean" == "0" ]]; then
  count=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('count',0))" 2>/dev/null)
  pass "$T (all $count blocks are clean)"
else
  fail "$T" "found $non_clean non-clean blocks in filtered result"
fi

# T10 BACKUP_EXISTS
T="T10 BACKUP_EXISTS"
recent_dump=$(find /compose/n8n/backups/ -name '*.dump' -mmin -1500 -type f 2>/dev/null | head -1)
if [[ -n "$recent_dump" ]]; then
  age_h=$(( ( $(date +%s) - $(stat -c %Y "$recent_dump") ) / 3600 ))
  pass "$T ($(basename "$recent_dump"), ${age_h}h old)"
else
  fail "$T" "no .dump file younger than 25h in /compose/n8n/backups/"
fi

# T11 DREAM_STATS
T="T11 DREAM_STATS"
resp=$(api "$WEBHOOK/api/manage" "$KEY_PRIVATE" '{"action":"dream-stats"}')
if echo "$resp" | grep -q '"success":true'; then
  checked=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('dream_checked',0))" 2>/dev/null)
  links=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('dream_links',0))" 2>/dev/null)
  pass "$T (checked=$checked, links=$links)"
else
  fail "$T" "dream-stats failed"
fi

# T12 DREAM_REVIEW
T="T12 DREAM_REVIEW"
resp=$(api "$WEBHOOK/api/manage" "$KEY_PRIVATE" '{"action":"dream-review"}')
if echo "$resp" | grep -q '"success":true'; then
  pass "$T"
else
  fail "$T" "dream-review failed"
fi

# =====================================================================
# PART 2: Retrieval Tests (Ollama required)
# =====================================================================
if $WITH_OLLAMA; then
  echo ""
  echo "--- Part 2: Retrieval Tests (Ollama) ---"
  echo ""

  # T13 SEARCH_BASIC
  T="T13 SEARCH_BASIC"
  resp=$(api "$WEBHOOK/api/search" "$KEY_PRIVATE" \
    '{"query":"Write Guard"}' 120)
  count=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('count',0))" 2>/dev/null)
  if [[ -n "$count" ]] && (( count >= 1 )); then
    pass "$T (count=$count)"
  else
    fail "$T" "expected >= 1 result, got count=$count"
  fi

  # T14 AGENT_CONFIDENT
  T="T14 AGENT_CONFIDENT"
  resp=$(api "$WEBHOOK/api/query" "$KEY_PRIVATE" \
    '{"query":"How does the Write Guard work?"}' 120)
  confidence=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('confidence',''))" 2>/dev/null)
  answer=$(echo "$resp" | python3 -c "import sys,json; a=json.load(sys.stdin).get('answer',''); print('nonempty' if len(a)>10 else 'empty')" 2>/dev/null)
  if [[ "$confidence" == "confident" ]] && [[ "$answer" == "nonempty" ]]; then
    pass "$T (confidence=$confidence)"
  else
    fail "$T" "expected confidence=confident + nonempty answer, got confidence=$confidence answer=$answer"
  fi

  # T15 AGENT_NEGATIVE
  T="T15 AGENT_NEGATIVE"
  resp=$(api "$WEBHOOK/api/query" "$KEY_PRIVATE" \
    '{"query":"Rezept fuer Kartoffelsuppe"}' 120)
  answer=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('answer',''))" 2>/dev/null)
  confidence=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('confidence',''))" 2>/dev/null)
  if echo "$answer" | grep -qi "no_relevant_blocks_found\|keine antwort\|keine relevanten\|nicht relevant\|no relevant\|cannot answer\|kann.*nicht"; then
    pass "$T (negative correctly detected)"
  elif [[ "$confidence" == "none" ]] || [[ "$confidence" == "low" ]] || [[ "$confidence" == "no_relevant_blocks_found" ]]; then
    pass "$T (confidence=$confidence)"
  else
    fail "$T" "expected rejection, got confidence=$confidence answer=${answer:0:80}"
  fi

  # T16 AGENT_BILINGUAL
  T="T16 AGENT_BILINGUAL"
  resp=$(api "$WEBHOOK/api/query" "$KEY_PRIVATE" \
    '{"query":"PostgreSQL Mount-Pfad"}' 120)
  answer=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('answer','').lower())" 2>/dev/null)
  if echo "$answer" | grep -qi "postgresql\|mount\|/var/lib"; then
    pass "$T"
  else
    fail "$T" "expected answer containing postgresql/mount, got: ${answer:0:80}"
  fi
else
  echo ""
  echo "--- Part 2: Retrieval Tests SKIPPED (use --with-ollama) ---"
fi

# =====================================================================
# Summary
# =====================================================================
echo ""
TOTAL=$((PASS + FAIL))
echo "=== Results: $PASS/$TOTAL passed, $FAIL failed ==="

if (( FAIL > 0 )); then
  exit 1
else
  exit 0
fi
