#!/usr/bin/env bash
#
# Context Store Benchmark Test Suite
# Usage: ./test.sh              — runs T01-T12 + T19-T22 (system tests only)
#        ./test.sh --with-ollama — runs all tests (adds T13-T18 retrieval + MCP + T23 digest)
#
# Test IDs are append-only: T19 (graph) and T20 (settings) live in Part 1
# because neither needs an LLM — T13-T18 predate them inside the
# --with-ollama block.
#
# A request without an answer passes no test (#478). Every request goes
# through eval_api_request (scripts/lib/eval-api.sh), which keeps the HTTP
# status and a --max-time: no connection, a timeout, a status outside 2xx or a
# body that is not the endpoint's answer (success:true; ok:true for the daily
# synthesis; a JSON-RPC result without isError for /mcp) fails a test that
# expects an answer, naming the class. A test whose promise is a refusal (T01,
# the 403 of T20/T21/T24, T22's unconfirmed downgrade, T24's classify backend,
# T19's 404) checks the status and ctx's error body of that refusal. The corpus
# gate is the first request: without an answer the run stops before any test.
# Exit status: 0 every test passed (SKIPs allowed), 1 a test failed or the
# corpus gate got no answer.
#
# ctx — Your AI's save game. By GottZ and Damien Moon (github.com/GottZ/ctx/graphs/contributors)
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
# Isolations-Fixture: eigener leerer Scope 'isotest' statt des 2026-08-06
# abgebauten work-Scopes — die Isolations-Tests (T03/T04, T19-404-Equality)
# brauchen IRGENDEINEN fremden home_scope, keinen mit Bestandsdaten.
KEY_ISO="${CONTEXT_API_KEY_ISOTEST:?CONTEXT_API_KEY_ISOTEST not set in .env}"
KEY_INVALID="deadbeef_invalid_key_0000000000000000000000000000000000000000"
# The request timeout of T17/T18 (/mcp: tools/list and the recent tool).
MCP_TIMEOUT_S=30

# DB container: env → running compose service `db` → exit 2 (issues #19, #40).
source "$SCRIPT_DIR/scripts/lib/db-container.sh"
ctx_resolve_db_container "$SCRIPT_DIR/docker-compose.yml" || exit 2

# shellcheck source=scripts/lib/eval-api.sh
if ! source "$SCRIPT_DIR/scripts/lib/eval-api.sh"; then
  echo "[FATAL] $SCRIPT_DIR/scripts/lib/eval-api.sh not found (test.sh runs from the repository root)"
  exit 1
fi

export PGPASSWORD="${CONTEXT_DB_PASSWORD:?CONTEXT_DB_PASSWORD not set in .env}"
DB_CMD="docker exec -e PGPASSWORD ${DB_CONTAINER} psql -U ${CONTEXT_DB_USER:-context_user} -d ${CONTEXT_DB:-context_store} -t -A"

PASS=0
FAIL=0
SKIP=0
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

skip() {
  echo "[SKIP] $1 -- $2"
  ((SKIP++))
}

# api URL KEY BODY [TIMEOUT_S] — POST BODY with X-Context-Key: KEY (timeout
# default 10 s) through eval_api_request. Sets, in this shell — call it
# directly, never inside $(…): API_BODY, API_STATUS (000 without a
# response), API_NO_ANSWER ('' for an answer, else its class) and API_DETAIL
# (the class in words). The key reaches curl on its stdin, off the process list.
api() {
  eval_api_request POST "$1" "$3" "${4:-10}" "X-Context-Key: $2"
}

# get URL KEY [TIMEOUT_S] — the same for a GET.
get() {
  eval_api_request GET "$1" "" "${3:-10}" "X-Context-Key: $2"
}

# answered T — true when the last request got an answer; otherwise a FAIL line
# for T that names the class, and false.
answered() {
  [[ -z "$API_NO_ANSWER" ]] && return 0
  fail "$1" "no answer: $API_DETAIL"
  return 1
}

# refused STATUS [NEEDLE] — whether the last request was refused the way ctx
# refuses: HTTP STATUS and a JSON object whose error is a non-empty string and
# whose success is not true; with NEEDLE, the error or the field errors name
# it. An empty reply, a 500 or a proxy's page is no refusal.
refused() {
  [[ "$API_STATUS" == "$1" ]] || return 1
  printf '%s' "$API_BODY" | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except ValueError:
    sys.exit(1)
ok = isinstance(d, dict) and d.get("success") is not True and isinstance(d.get("error"), str) and d["error"] != ""
if ok and sys.argv[1]:
    ok = sys.argv[1] in d["error"] or sys.argv[1] in json.dumps(d.get("fields"), ensure_ascii=False)
sys.exit(0 if ok else 1)' "${2:-}" 2>/dev/null
}

# seen — the last answer in words, on one line, for a FAIL line.
seen() {
  local b="${API_BODY:0:100}"
  b="${b//$'\r'/}"
  printf '%s %s' "${API_DETAIL:-HTTP $API_STATUS}" "${b//$'\n'/ }"
}

# Cleanup trap: always delete temp block
cleanup() {
  if [[ -n "$CLEANUP_ID" ]]; then
    api "$WEBHOOK/api/manage" "$KEY_PRIVATE" "{\"action\":\"delete\",\"id\":\"$CLEANUP_ID\"}" 10
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
db_status=$(docker inspect --format '{{.State.Health.Status}}' "$DB_CONTAINER" 2>/dev/null || echo "unknown")
echo "Config: db=$DB_CONTAINER ($db_status)"
echo ""

# Corpus gate: corpus-bound tests (T02, T10, T13, T14, T19, T23) assume a
# grown store (home deployment: thousands of blocks). Fresh deployments would
# fail them spuriously — below CORPUS_MIN blocks they SKIP instead. Behavior
# tests (auth, CRUD, settings, …) always run. Same stats surface as T02.
# The gate is the run's first request: without an answer (or an answer without
# stats.total_blocks) no test would mean anything, so the run stops here with
# exit 1 — a SKIP stays for a store that answered with fewer blocks (#478).
CORPUS_MIN=100
api "$WEBHOOK/api/manage" "$KEY_PRIVATE" '{"action":"stats"}'
if [[ -n "$API_NO_ANSWER" ]]; then
  if [[ "$API_STATUS" == 401 ]]; then
    echo "[FATAL] corpus gate: $WEBHOOK answered HTTP 401 — CONTEXT_API_KEY_PRIVATE is not a key of that server; no test ran"
  else
    echo "[FATAL] corpus gate: $WEBHOOK/api/manage stats got no answer ($API_DETAIL); no test ran"
  fi
  echo "=== Results: no test ran, the corpus gate got no answer ==="
  exit 1
fi
CORPUS_TOTAL=$(printf '%s' "$API_BODY" | python3 -c "
import sys, json
n = json.load(sys.stdin)['stats']['total_blocks']
print(n if isinstance(n, int) and not isinstance(n, bool) else '')" 2>/dev/null)
if [[ -z "$CORPUS_TOTAL" ]]; then
  echo "[FATAL] corpus gate: $WEBHOOK/api/manage stats answered without an integer stats.total_blocks; no test ran"
  echo "=== Results: no test ran, the corpus gate got no answer ==="
  exit 1
fi
CORPUS_OK=true
if (( CORPUS_TOTAL < CORPUS_MIN )); then
  CORPUS_OK=false
  echo "Corpus: total_blocks=$CORPUS_TOTAL < $CORPUS_MIN — corpus-bound tests will SKIP"
  echo ""
fi

# =====================================================================
# PART 1: System Tests (no Ollama)
# =====================================================================
echo "--- Part 1: System Tests ---"
echo ""

# T00 VET_INTEGRATION (G28 preflight) — go test -short and golangci NEVER
# compile build-tagged _test.go files; a signature drift there ships
# invisibly and only the CI matrix goes red (v3.5.0 lesson, 019ebdb4-65e0).
# This is the local mirror of that CI leg. Skipped when go is unavailable.
if command -v go >/dev/null 2>&1 && [[ -d "$SCRIPT_DIR/go" ]]; then
  T="T00 VET_INTEGRATION"
  if (cd "$SCRIPT_DIR/go" && GOTMPDIR="$SCRIPT_DIR/.gocache" GOCACHE="$SCRIPT_DIR/.gocache/build" go vet -tags=integration ./... >/dev/null 2>&1); then
    pass "$T"
  else
    fail "$T" "go vet -tags=integration failed — run it in go/ for details"
  fi
fi

# T01 AUTH_REJECT
T="T01 AUTH_REJECT"
# The promise is the refusal itself: HTTP 401 with ctx's error body (handler.Auth).
api "$WEBHOOK/api/manage" "$KEY_INVALID" '{"action":"stats"}'
if refused 401 unauthorized; then
  pass "$T"
else
  fail "$T" "expected HTTP 401 with error unauthorized, got: $(seen)"
fi

# T02 AUTH_PRIVATE
T="T02 AUTH_PRIVATE"
if ! $CORPUS_OK; then
  skip "$T" "corpus-bound (total_blocks=${CORPUS_TOTAL:-unknown} < $CORPUS_MIN)"
else
api "$WEBHOOK/api/manage" "$KEY_PRIVATE" '{"action":"stats"}'
if answered "$T"; then
total=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin)['stats']['total_blocks'])" 2>/dev/null)
if [[ -n "$total" ]] && (( total >= 180 )); then
  pass "$T (total_blocks=$total)"
else
  fail "$T" "expected total_blocks >= 180, got: $total"
fi
fi
fi

# T03 AUTH_ISOTEST
T="T03 AUTH_ISOTEST"
api "$WEBHOOK/api/manage" "$KEY_ISO" '{"action":"stats"}'
if answered "$T"; then
total=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin)['stats']['total_blocks'])" 2>/dev/null)
if [[ -n "$total" ]] && (( total < 30 )); then
  pass "$T (total_blocks=$total)"
else
  fail "$T" "expected total_blocks < 30 (scope isolation), got: $total"
fi
fi

# T04 SCOPE_ISOLATION — guard-list is open to every key and answers the
# blocks of the key's read scopes: the isotest key must get an answer with a
# blocks list that holds no private block. No answer is no evidence of
# isolation (an empty reply used to pass here).
T="T04 SCOPE_ISOLATION"
api "$WEBHOOK/api/manage" "$KEY_ISO" '{"action":"guard-list"}'
if answered "$T"; then
  has_private=$(printf '%s' "$API_BODY" | python3 -c "
import sys,json
blocks=json.load(sys.stdin).get('blocks')
print(len([b for b in blocks if b.get('scope')=='private']) if isinstance(blocks, list) else '')
" 2>/dev/null)
  if [[ "$has_private" == "0" ]]; then
    pass "$T"
  elif [[ -z "$has_private" ]]; then
    fail "$T" "the answer carries no blocks list: ${API_BODY:0:100}"
  else
    fail "$T" "found $has_private private-scope blocks via the isotest key"
  fi
fi

# T05 CRUD_LIFECYCLE
T="T05 CRUD_LIFECYCLE"
t05_ok=true
t05_msg=""

# Save
api "$WEBHOOK/api/store" "$KEY_PRIVATE" \
  "{\"category\":\"test\",\"title\":\"$TEST_TITLE\",\"content\":\"benchmark crud test content $(date +%s)\",\"tags\":[\"benchmark\"]}"
if [[ -n "$API_NO_ANSWER" ]]; then
  t05_ok=false; t05_msg="save: no answer: $API_DETAIL"
fi

# Search to get ID
if $t05_ok; then
  sleep 1
  api "$WEBHOOK/api/search" "$KEY_PRIVATE" "{\"query\":\"$TEST_TITLE\"}"
  if [[ -n "$API_NO_ANSWER" ]]; then
    t05_ok=false; t05_msg="search: no answer: $API_DETAIL"
  else
    CLEANUP_ID=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin)['results'][0]['id'])" 2>/dev/null)
    if [[ -z "$CLEANUP_ID" ]]; then
      t05_ok=false; t05_msg="search returned no results for $TEST_TITLE"
    fi
  fi
fi

# Get by ID
if $t05_ok; then
  api "$WEBHOOK/api/manage" "$KEY_PRIVATE" "{\"action\":\"get\",\"id\":\"$CLEANUP_ID\"}"
  if [[ -n "$API_NO_ANSWER" ]]; then
    t05_ok=false; t05_msg="get: no answer: $API_DETAIL"
  else
    got_title=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin)['block']['title'])" 2>/dev/null)
    if [[ "$got_title" != "$TEST_TITLE" ]]; then
      t05_ok=false; t05_msg="get returned wrong title: $got_title"
    fi
  fi
fi

# Delete
if $t05_ok; then
  api "$WEBHOOK/api/manage" "$KEY_PRIVATE" "{\"action\":\"delete\",\"id\":\"$CLEANUP_ID\"}"
  if [[ -z "$API_NO_ANSWER" ]]; then
    CLEANUP_ID=""  # already cleaned up
  else
    t05_ok=false; t05_msg="delete: no answer: $API_DETAIL"
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
api "$WEBHOOK/api/store" "$KEY_PRIVATE" \
  "{\"category\":\"test\",\"title\":\"$t06_title\",\"content\":\"upsert noop test content\",\"tags\":[\"benchmark\"]}"
if [[ -n "$API_NO_ANSWER" ]]; then
  t06_ok=false; t06_msg="first save: no answer: $API_DETAIL"
fi

# Second save (identical content)
if $t06_ok; then
  api "$WEBHOOK/api/store" "$KEY_PRIVATE" \
    "{\"category\":\"test\",\"title\":\"$t06_title\",\"content\":\"upsert noop test content\",\"tags\":[\"benchmark\"]}"
  if [[ -n "$API_NO_ANSWER" ]]; then
    t06_ok=false; t06_msg="second save: no answer: $API_DETAIL"
  else
    action=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('action',''))" 2>/dev/null)
    t06_id=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('existing_id',''))" 2>/dev/null)
    if [[ "$action" == "noop" ]]; then
      pass "$T (action=noop)"
    else
      t06_ok=false; t06_msg="expected action=noop, got: $action"
    fi
  fi
fi

# Cleanup
if [[ -n "$t06_id" ]]; then
  api "$WEBHOOK/api/manage" "$KEY_PRIVATE" "{\"action\":\"delete\",\"id\":\"$t06_id\"}" 10
else
  # Try to find and delete by search
  sleep 1
  api "$WEBHOOK/api/search" "$KEY_PRIVATE" "{\"query\":\"$t06_title\"}"
  found_id=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; r=json.load(sys.stdin).get('results',[]); print(r[0]['id'] if r else '')" 2>/dev/null)
  [[ -n "$found_id" ]] && api "$WEBHOOK/api/manage" "$KEY_PRIVATE" "{\"action\":\"delete\",\"id\":\"$found_id\"}" 10
fi

if ! $t06_ok; then
  fail "$T" "$t06_msg"
fi

# T07 SCHEMA_INTEGRITY — Evokoa-Clean-Room design/03 §4.6/§7 W03-6: the
# handgepflegte COUNT-Check (46 tables / 40 context_blocks columns, ~80
# Zeilen Änderungs-Chronik, zwei dokumentierte Stale-Vorfälle — Historie in
# git blame dieser Datei) ist durch `ctx contract` abgelöst. Ein generiertes
# Manifest (internal/schemacontract, go/internal/schemacontract/manifest.json)
# ersetzt die Hand-Erwartung strukturell: neue Migrationen ändern das
# Manifest per Regeneration, keine Zeile hier muss je wieder von Hand
# nachgezogen werden. `ctx contract` ruft per Default `?refresh=1` — das
# Gate validiert den IST-Stand, nie einen gecachten Boot-Report.
#
# Fallback (KEINE Gate-Abschwächung): solange die installierte CLI älter
# als W03-4 ist ("contract" unbekannt) oder Transport/Auth fehlschlägt,
# fällt T07 auf exakt die alte Zähl-Probe zurück (53 Tabellen / 41 Spalten
# context_blocks) — dieselbe Prüfstärke wie vor dieser Welle, nicht
# schwächer. Das Upgrade auf den generierten Contract greift automatisch,
# sobald die deployte CLI "contract" kennt — kein Script-Change nötig.
#
# ⚠ Die Fallback-Zahlen sind handgepflegt und hängen am Migrations-Stand:
# 58/42 gilt ab Migration 135 (distill_run + distill_seen — Lauf-Journal und
# Dedup-Ledger des Distillers, Achse 03/W03-1); 56/42 ab Migration 134 und
# 132 (context_embed_cache_meta — der Boot-Abgleich des
# Embed-Caches, Achse 04/A04-W4); 55/42 ab Mig 131 (dream_last_inert);
# 55/41 gilt ab Migration 130 (graph_overview_run — das Lauf-Journal der
# Wurzel-Map, Achse 04/S2); 54/41 ab Mig 129 (context_digest_state fällt;
# 54/41 ab Mig 127 mit graph_cluster_topic_edge + _super + _super_member;
# davor 51/41 ab Mig 124, 50/41 ab Mig 118,
# 46/40 ab Mig 107). Bei neuen Migrationen mit Tabellen-/Spalten-Änderung
# müssen sie hier nachgezogen werden — der `ctx contract`-Pfad oben braucht
# das nicht. Gegen das stille Vergessen steht seit Migration 124 ein Gate:
# store/cluster_topic_schema_integration_test.go zählt dieselben zwei Werte
# gegen eine frisch migrierte DB und wird rot, wenn sie hier auseinanderlaufen.
T="T07 SCHEMA_INTEGRITY"

# Zähl-Fallback-Erwartung, an Migration 170 gebunden (siehe Kommentar oben;
# 131 fügt context_blocks.dream_last_inert hinzu → 42 Spalten; 132 fügt die
# Tabelle context_embed_cache_meta hinzu → 56 Tabellen; 135 fügt distill_run
# und distill_seen hinzu → 58 Tabellen, Spalten unverändert; 159 fügt
# context_llm_usage_hourly hinzu → 59 Tabellen, Spalten unverändert; 161 fügt
# context_model_consumers und context_model_keys hinzu → 61 Tabellen, Spalten
# unverändert; 162 fügt context_model_aliases hinzu → 62 Tabellen, Spalten
# unverändert; 163 fügt context_instance hinzu → 63 Tabellen, Spalten
# unverändert; 164 fügt context_model_consumer_grants hinzu → 64 Tabellen,
# Spalten unverändert; 165 und 166 fügen keine Tabelle hinzu → 64 Tabellen,
# Spalten unverändert; 167 fügt context_llm_rejections_hourly hinzu → 65
# Tabellen, Spalten unverändert; 168 fügt context_gateway_limits hinzu → 66
# Tabellen, Spalten unverändert; 169 fügt keine Tabelle hinzu; 170 fügt
# context_block_counts und context_block_count_deltas hinzu → 68 Tabellen,
# Spalten unverändert; 171 und 172 fügen keine Tabelle hinzu; 173 fügt
# context_blocks.embedding_stale hinzu → 68 Tabellen, 43 Spalten; 174 fügt
# context_blocks.untrusted_origin hinzu → 68 Tabellen, 44 Spalten; 175 fügt
# context_blocks.embed_excluded und context_embed_excluded_reconcile hinzu →
# 69 Tabellen, 45 Spalten; 176 fügt context_embed_excluded_queue hinzu → 70
# Tabellen, Spalten unverändert; 189 fügt context_temporal_reviews hinzu → 71
# Tabellen, Spalten unverändert; 190 fügt context_temporal_reviews.extraction
# hinzu → 71 Tabellen, context_blocks-Spalten unverändert; 191 fügt keine
# Tabelle hinzu; 192 fügt context_install hinzu → 72 Tabellen, Spalten
# unverändert).
T07_EXPECT_TABLES=72
T07_EXPECT_COLUMNS=45

t07_count_fallback() {
  local reason="$1"
  local tc col
  tc=$($DB_CMD -c "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name NOT LIKE '%_snapshot_%';" 2>/dev/null | tr -d '[:space:]')
  col=$($DB_CMD -c "SELECT count(*) FROM information_schema.columns WHERE table_name='context_blocks';" 2>/dev/null | tr -d '[:space:]')
  if [[ "$tc" == "$T07_EXPECT_TABLES" ]] && [[ "$col" == "$T07_EXPECT_COLUMNS" ]]; then
    pass "$T (contract-CLI nicht verfügbar — Zähl-Fallback (Prä-Deploy); tables=$tc, columns=$col; $reason)"
  else
    fail "$T" "Zähl-Fallback (Prä-Deploy, $reason): expected $T07_EXPECT_TABLES tables + $T07_EXPECT_COLUMNS columns (Stand Mig 192), got tables=$tc columns=$col"
  fi
}

# set -e-sicher: der if/else fängt den Exit-Code ab, BEVOR eine Pipe/Zuweisung
# ihn verschlucken könnte (Projekt-Quirk "EXIT nach Pipe misst die Pipe";
# dasselbe Muster wie state.sh's Contract-Zeile, W03-5).
# CTX_KEY-Override: die CLI zieht ihren Key sonst aus ~/.config/ctx/config
# (Private-Key) — /api/contract ist admin-gated, das gäbe HTTP 403 → Exit 3 →
# stiller Abstieg in den Zähl-Fallback. Env schlägt Datei (internal/cli:
# LoadConfig, ENV > ~/.config/ctx/config); CTX_BASE_URL bleibt aus der Datei.
# CLI-Guard (issue #19): ohne installierte ctx-CLI wäre das ein Exit-127-
# Rauschen im Zähl-Fallback — expliziter SKIP statt Pseudo-Diagnose.
if ! command -v ctx >/dev/null 2>&1; then
  skip "$T" "ctx CLI not installed (command -v ctx empty)"
  t07_rc="skipped"
elif t07_out=$(CTX_KEY="${CTX_ADMIN_KEY:-}" ctx contract 2>&1); then
  t07_rc=0
else
  t07_rc=$?
fi

case "$t07_rc" in
  skipped)
    : # already counted via skip()
    ;;
  0)
    pass "$T (ctx contract: ok)"
    ;;
  1)
    if [[ "$t07_out" == *"unknown command"* ]]; then
      t07_count_fallback "Exit 1/unknown command — installierte CLI kennt \"contract\" nicht, Vor-W03-4-Stand"
    else
      fail "$T" "ctx contract: DRIFT -- $(echo "$t07_out" | head -5 | tr '\n' ' ')"
    fi
    ;;
  2)
    fail "$T" "ctx contract: UNCHECKED -- $(echo "$t07_out" | head -5 | tr '\n' ' ')"
    ;;
  3)
    t07_count_fallback "Exit 3/Transport-Auth-Fehler"
    ;;
  *)
    t07_count_fallback "unerwarteter Exit $t07_rc"
    ;;
esac

# T08 GUARD_STATS
T="T08 GUARD_STATS"
api "$WEBHOOK/api/manage" "$KEY_PRIVATE" '{"action":"guard-stats"}'
if answered "$T"; then
  pass "$T"
fi

# T09 GUARD_LIST_FILTER — an answer with a blocks list, every block clean (an
# error body without blocks used to pass as "0 non-clean").
T="T09 GUARD_LIST_FILTER"
api "$WEBHOOK/api/manage" "$KEY_PRIVATE" '{"action":"guard-list","status":"clean"}'
if answered "$T"; then
non_clean=$(printf '%s' "$API_BODY" | python3 -c "
import sys,json
blocks=json.load(sys.stdin).get('blocks')
print(len([b for b in blocks if b.get('guard_status')!='clean']) if isinstance(blocks, list) else '')
" 2>/dev/null)
if [[ "$non_clean" == "0" ]]; then
  count=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('count',0))" 2>/dev/null)
  pass "$T (all $count blocks are clean)"
elif [[ -z "$non_clean" ]]; then
  fail "$T" "the answer carries no blocks list: ${API_BODY:0:100}"
else
  fail "$T" "found $non_clean non-clean blocks in filtered result"
fi
fi

# T10 BACKUP_EXISTS
T="T10 BACKUP_EXISTS"
if ! $CORPUS_OK; then
  skip "$T" "corpus-bound (total_blocks=${CORPUS_TOTAL:-unknown} < $CORPUS_MIN)"
else
recent_dump=$(find "$SCRIPT_DIR/backups/" -name '*.dump' -mmin -1500 -type f 2>/dev/null | head -1)
if [[ -n "$recent_dump" ]]; then
  age_h=$(( ( $(date +%s) - $(stat -c %Y "$recent_dump") ) / 3600 ))
  pass "$T ($(basename "$recent_dump"), ${age_h}h old)"
else
  fail "$T" "no .dump file younger than 25h in $SCRIPT_DIR/backups/"
fi
fi

# T11 DREAM_STATS
T="T11 DREAM_STATS"
api "$WEBHOOK/api/manage" "$KEY_PRIVATE" '{"action":"dream-stats"}'
if answered "$T"; then
  checked=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('dream_checked',0))" 2>/dev/null)
  links=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('dream_links',0))" 2>/dev/null)
  pass "$T (checked=$checked, links=$links)"
fi

# T12 DREAM_REVIEW
T="T12 DREAM_REVIEW"
api "$WEBHOOK/api/manage" "$KEY_PRIVATE" '{"action":"dream-review"}'
if answered "$T"; then
  pass "$T"
fi

# T19 GRAPH_EGO — GET /api/graph/ego (G07/F5-W1). Four assertions per the
# wave gate: node/edge counts > 0, NO content field anywhere in the payload,
# stats.truncated present, and 404-equality (invisible private block vs
# nonexistent UUID must answer byte-identically — no existence oracle).
T="T19 GRAPH_EGO"
if ! $CORPUS_OK; then
  skip "$T" "corpus-bound (total_blocks=${CORPUS_TOTAL:-unknown} < $CORPUS_MIN)"
else
# Focus: the highest-degree non-archived private/shared block (visible to
# KEY_PRIVATE, guaranteed >= 1 edge so the edge-count assertion is meaningful).
# Typen mit retrieval-Policy 'excluded' scheiden generisch aus — /api/graph/ego
# antwortet für sie 404. Kein Hardcode mehr ('system-meta' war die Liste bis
# M107 den Typ 'checkpoint' einführte, der prompt den Top-Grad-Block stellte):
# der JOIN liest die Policy aus der Registry. context_block_types ist
# scope-qualifiziert (name+scope unique); '_global' ist die Basis-Ebene, die
# jeder Tenant erbt. LEFT JOIN, damit Blöcke ohne Registry-Zeile drinbleiben.
t19_focus=$($DB_CMD -c "SELECT b.id FROM context_blocks b
  JOIN context_dream_links l ON l.source_block_id = b.id OR l.target_block_id = b.id
  LEFT JOIN context_block_types t ON t.name = b.type_name AND t.scope = '_global'
  WHERE NOT b.is_archived AND b.scope IN ('private','shared')
    AND t.config->'retrieval'->>'policy' IS DISTINCT FROM 'excluded'
  GROUP BY b.id ORDER BY count(*) DESC LIMIT 1;" 2>/dev/null | tr -d '[:space:]')
if [[ -z "$t19_focus" ]]; then
  fail "$T" "no linked block found to use as focus"
else
  get "$WEBHOOK/api/graph/ego?block=$t19_focus&hops=2" "$KEY_PRIVATE" 15
  if answered "$T"; then
  t19_check=$(printf '%s' "$API_BODY" | python3 -c "
import sys, json
d = json.load(sys.stdin)
stats = d.get('stats', {})
problems = []
if not d.get('success'): problems.append('success!=true')
if stats.get('nodes', 0) < 1: problems.append('nodes=0')
if stats.get('edges', 0) < 1: problems.append('edges=0')
if 'truncated' not in stats: problems.append('stats.truncated missing')
if any('content' in n for n in d.get('nodes', [])): problems.append('content field leaked')
print(';'.join(problems) if problems else 'OK ' + str(stats.get('nodes')) + ' nodes ' + str(stats.get('edges')) + ' edges')
" 2>/dev/null)
  if [[ "$t19_check" != OK* ]]; then
    fail "$T" "payload check: ${t19_check:-unparseable response: ${API_BODY:0:100}}"
  else
    # 404-equality: a private block (invisible to KEY_ISO) and a nonexistent
    # UUID must produce byte-identical 404 bodies — each ctx's error body.
    t19_priv=$($DB_CMD -c "SELECT id FROM context_blocks WHERE scope='private' AND NOT is_archived LIMIT 1;" 2>/dev/null | tr -d '[:space:]')
    t19_missing="00000000-0000-7000-8000-000000000000"
    get "$WEBHOOK/api/graph/ego?block=$t19_priv" "$KEY_ISO" 10
    t19_ok1=false; refused 404 && t19_ok1=true
    t19_code1="$API_STATUS"; t19_body1="$API_BODY"
    get "$WEBHOOK/api/graph/ego?block=$t19_missing" "$KEY_ISO" 10
    t19_ok2=false; refused 404 && t19_ok2=true
    t19_code2="$API_STATUS"; t19_body2="$API_BODY"
    if $t19_ok1 && $t19_ok2 && [[ "$t19_body1" == "$t19_body2" ]]; then
      pass "$T (${t19_check#OK }, 404-equality holds)"
    else
      fail "$T" "404-oracle: invisible=$t19_code1 missing=$t19_code2 error_bodies=$($t19_ok1 && $t19_ok2 && echo yes || echo no) bodies_equal=$([[ "$t19_body1" == "$t19_body2" ]] && echo yes || echo no)"
    fi
  fi
  fi
fi
fi

# T20 SETTINGS_ROUNDTRIP — /api/settings (G16/F2-W5). Admin-gated runtime
# overrides: non-admin GET must 403 (F4-O4); PUT flips the source to db and
# DELETE reverts it. The PUT writes the CURRENT effective value, so an
# aborted run between PUT and DELETE never changes live behavior.
T="T20 SETTINGS_ROUNDTRIP"
if [[ -z "${CTX_ADMIN_KEY:-}" ]]; then
  fail "$T" "CTX_ADMIN_KEY not set in .env (host-only admin key, G09 bootstrap)"
else
  get "$WEBHOOK/api/settings" "$KEY_PRIVATE" 10
  t20_403=false; refused 403 && t20_403=true
  t20_403_seen=$(seen)
  get "$WEBHOOK/api/settings/rerank.blend_weight" "$CTX_ADMIN_KEY" 10
  t20_cur=""; t20_cur_na="$API_DETAIL"
  [[ -z "$API_NO_ANSWER" ]] && t20_cur=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin)['setting']['value'])" 2>/dev/null)
  if ! $t20_403; then
    fail "$T" "non-admin GET expected HTTP 403 with an error body, got: $t20_403_seen"
  elif [[ -z "$t20_cur" ]]; then
    fail "$T" "could not read current rerank.blend_weight via admin key${t20_cur_na:+ (no answer: $t20_cur_na)}"
  else
    eval_api_request PUT "$WEBHOOK/api/settings/rerank.blend_weight" "{\"value\":$t20_cur}" 15 "X-Context-Key: $CTX_ADMIN_KEY"
    t20_src=""; t20_put_na="$API_DETAIL"
    [[ -z "$API_NO_ANSWER" ]] && t20_src=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('source',''))" 2>/dev/null)
    # The DELETE runs whatever the PUT answered: a PUT without an answer may
    # still have written the override.
    eval_api_request DELETE "$WEBHOOK/api/settings/rerank.blend_weight" "" 15 "X-Context-Key: $CTX_ADMIN_KEY"
    t20_dsrc=""; t20_del_na="$API_DETAIL"
    [[ -z "$API_NO_ANSWER" ]] && t20_dsrc=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('source',''))" 2>/dev/null)
    if [[ "$t20_src" == "db" && -n "$t20_dsrc" && "$t20_dsrc" != "db" ]]; then
      pass "$T (403 enforced, override db→$t20_dsrc, value=$t20_cur)"
    else
      fail "$T" "put_source=${t20_src:-${t20_put_na:-unparseable}} delete_source=${t20_dsrc:-${t20_del_na:-unparseable}}"
    fi
  fi
fi

# T21 BACKEND_POOL (F3-P1/M053) — bootstrap seeded the pool from the env-era
# config, backend-list is admin-gated (it discloses egress topology), and the
# rows carry sane trust/locality. No LLM needed: pure pool/manage surface.
T="T21 BACKEND_POOL"
if [[ -z "${CTX_ADMIN_KEY:-}" ]]; then
  fail "$T" "CTX_ADMIN_KEY not set in .env (host-only admin key)"
else
  api "$WEBHOOK/api/manage" "$KEY_PRIVATE" '{"action":"backend-list"}'
  t21_403=false; refused 403 && t21_403=true
  t21_403_seen=$(seen)
  api "$WEBHOOK/api/manage" "$CTX_ADMIN_KEY" '{"action":"backend-list"}'
  t21_count=""; t21_na="$API_DETAIL"
  [[ -z "$API_NO_ANSWER" ]] && t21_count=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['backends']))" 2>/dev/null)
  if ! $t21_403; then
    fail "$T" "non-admin backend-list expected HTTP 403 with an error body, got: $t21_403_seen"
  elif [[ -z "$t21_count" || "$t21_count" -lt 3 ]]; then
    fail "$T" "expected >=3 bootstrapped backends, got ${t21_count:-${t21_na:+no answer: }${t21_na:-unparseable}}"
  else
    pass "$T (403 enforced, $t21_count pool rows)"
  fi
fi

# T22 BLOCK_SENSITIVITY (F3-P3/M055) — store with explicit sensitivity stamps
# source=manual, an unconfirmed downgrade is rejected (the confirm-gated
# direction), a confirmed one applies + audits. No LLM: pure write surface.
T="T22 BLOCK_SENSITIVITY"
api "$WEBHOOK/api/store" "$KEY_PRIVATE" \
  '{"category":"reference","title":"__t22_sensitivity_probe","content":"sensitivity gate probe '"$(date +%s)"'","sensitivity":"personal"}' 15
t22_id=""; t22_sens=""
if [[ -z "$API_NO_ANSWER" ]]; then
  t22_id=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; b=json.load(sys.stdin).get('block') or {}; print(b.get('id',''))" 2>/dev/null)
  t22_sens=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; b=json.load(sys.stdin).get('block') or {}; print(b.get('sensitivity',''), b.get('sensitivity_source',''))" 2>/dev/null)
fi
if [[ -n "$API_NO_ANSWER" ]]; then
  fail "$T" "store with sensitivity: no answer: $API_DETAIL"
elif [[ -z "$t22_id" ]]; then
  fail "$T" "store with sensitivity answered without a block id: ${API_BODY:0:200}"
elif [[ "$t22_sens" != "personal manual" ]]; then
  fail "$T" "expected sensitivity='personal manual', got '$t22_sens'"
else
  # The promise of this step is the refusal: HTTP 400 with an error naming
  # confirm_sensitivity_downgrade (manage update, applySensitivityGuard).
  api "$WEBHOOK/api/manage" "$KEY_PRIVATE" \
    '{"action":"update","id":"'"$t22_id"'","data":{"sensitivity":"public"}}' 15
  t22_deny_ok=false; refused 400 confirm_sensitivity_downgrade && t22_deny_ok=true
  t22_deny_seen=$(seen)
  api "$WEBHOOK/api/manage" "$KEY_PRIVATE" \
    '{"action":"update","id":"'"$t22_id"'","data":{"sensitivity":"public","confirm_sensitivity_downgrade":true}}' 15
  t22_conf_state="no answer: $API_DETAIL"
  [[ -z "$API_NO_ANSWER" ]] && t22_conf_state=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; d=json.load(sys.stdin); b=d.get('block') or {}; a=(b.get('metadata') or {}).get('sensitivity_audit') or {}; print(b.get('sensitivity',''), 'audited' if a.get('from')=='personal' and a.get('to')=='public' else 'unaudited')" 2>/dev/null)
  api "$WEBHOOK/api/manage" "$KEY_PRIVATE" '{"action":"delete","id":"'"$t22_id"'"}' 15
  if ! $t22_deny_ok; then
    fail "$T" "unconfirmed downgrade expected HTTP 400 naming confirm_sensitivity_downgrade, got: $t22_deny_seen"
  elif [[ "$t22_conf_state" != "public audited" ]]; then
    fail "$T" "confirmed downgrade expected 'public audited', got '$t22_conf_state'"
  else
    pass "$T (manual stamp, downgrade guard + audit)"
  fi
fi

# T24 SENSITIVITY_AUDIT_SURFACE (G41) — blocks-audit-status is admin-gated
# (titles + classification topology), the status carries by_source counts,
# and the classify role is hard-local: backend-create with classify+external
# is a 422, no metadata escape hatch. No LLM: pure manage/validation surface.
T="T24 SENSITIVITY_AUDIT_SURFACE"
if [[ -z "${CTX_ADMIN_KEY:-}" ]]; then
  fail "$T" "CTX_ADMIN_KEY not set in .env (host-only admin key)"
else
  api "$WEBHOOK/api/manage" "$KEY_PRIVATE" '{"action":"blocks-audit-status"}'
  t24_403=false; refused 403 && t24_403=true
  t24_403_seen=$(seen)
  api "$WEBHOOK/api/manage" "$CTX_ADMIN_KEY" '{"action":"blocks-audit-status"}'
  t24_status="no answer: $API_DETAIL"
  [[ -z "$API_NO_ANSWER" ]] && t24_status=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; d=json.load(sys.stdin); print('ok' if 'pending' in d and isinstance(d.get('by_source'),dict) else 'malformed')" 2>/dev/null)
  # The promise of this step is the refusal: HTTP 422 whose field errors name
  # the classify role (backends.Validate, writeBackendValidation).
  api "$WEBHOOK/api/manage" "$CTX_ADMIN_KEY" \
    '{"action":"backend-create","data":{"name":"__t24_classify_probe","base_url":"https://api.example.com/v1","protocol":"openai","provider_class":"generic","trust":"public","locality":"external","roles":["classify"],"model_map":{"default":"m"},"confirm_trust_elevation":true,"metadata":{"embed_equivalence_verified":true}}}'
  t24_extern_ok=false; refused 422 classify && t24_extern_ok=true
  t24_extern_seen=$(seen)
  if [[ -z "$API_NO_ANSWER" ]]; then
    # The probe row was created: remove it before failing.
    t24_pid=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; b=json.load(sys.stdin).get('backend') or {}; print(b.get('id',''))" 2>/dev/null)
    [[ -n "$t24_pid" ]] && api "$WEBHOOK/api/manage" "$CTX_ADMIN_KEY" '{"action":"backend-delete","id":"'"$t24_pid"'"}'
  fi
  if ! $t24_403; then
    fail "$T" "non-admin blocks-audit-status expected HTTP 403 with an error body, got: $t24_403_seen"
  elif [[ "$t24_status" != "ok" ]]; then
    fail "$T" "admin status: ${t24_status:-unparseable}"
  elif ! $t24_extern_ok; then
    fail "$T" "classify+external backend-create expected HTTP 422 naming classify, got: $t24_extern_seen"
  else
    pass "$T (403 enforced, status shape, classify hard-local 422)"
  fi
fi

# T25 OPENAI_GATEWAY (W02-15) — the OpenAI-compatible API against the running
# stack through the official Python SDK: go/conformance/oai/live/smoke.py t25
# creates a test consumer (all_models), checks /v1/models, chat with and
# without streaming, embeddings, rerank, the 401 without a key and the error
# envelope, and deletes the consumer. Without gateway.enabled (the default)
# the script exits 77 and T25 is a SKIP with that reason — never a PASS.
# Models come from /v1/models by kind unless T25_CHAT_MODEL /
# T25_EMBED_MODEL / T25_RERANK_MODEL name them. Needs uv (the SDK is pinned
# like the conformance harness: openai==3.42.1).
T="T25 OPENAI_GATEWAY"
if [[ -z "${CTX_ADMIN_KEY:-}" ]]; then
  fail "$T" "CTX_ADMIN_KEY not set in .env (host-only admin key)"
elif ! command -v uv >/dev/null 2>&1; then
  skip "$T" "uv not on PATH — the SDK smoke cannot run here"
else
  t25_models=()
  [[ -n "${T25_CHAT_MODEL:-}" ]] && t25_models+=(--chat-model "$T25_CHAT_MODEL")
  [[ -n "${T25_EMBED_MODEL:-}" ]] && t25_models+=(--embed-model "$T25_EMBED_MODEL")
  [[ -n "${T25_RERANK_MODEL:-}" ]] && t25_models+=(--rerank-model "$T25_RERANK_MODEL")
  t25_out=$(CTX_ADMIN_KEY="$CTX_ADMIN_KEY" uv run --no-project --quiet --with openai==3.42.1 \
    python3 "$SCRIPT_DIR/go/conformance/oai/live/smoke.py" t25 --base-url "$WEBHOOK" \
    --admin-key-env CTX_ADMIN_KEY "${t25_models[@]}" 2>&1)
  t25_rc=$?
  t25_last=$(printf '%s\n' "$t25_out" | awk 'NF' | tail -n 1)
  case $t25_rc in
    0) pass "$T (${t25_last#PASS: })" ;;
    77) skip "$T" "${t25_last#SKIP: }" ;;
    *) fail "$T" "rc=$t25_rc ${t25_last:-no output}" ;;
  esac
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
  if ! $CORPUS_OK; then
    skip "$T" "corpus-bound (total_blocks=${CORPUS_TOTAL:-unknown} < $CORPUS_MIN)"
  else
  api "$WEBHOOK/api/search" "$KEY_PRIVATE" '{"query":"Write Guard"}' 120
  if answered "$T"; then
  count=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('count',0))" 2>/dev/null)
  if [[ -n "$count" ]] && (( count >= 1 )); then
    pass "$T (count=$count)"
  else
    fail "$T" "expected >= 1 result, got count=$count"
  fi
  fi
  fi

  # T14 AGENT_CONFIDENT
  T="T14 AGENT_CONFIDENT"
  if ! $CORPUS_OK; then
    skip "$T" "corpus-bound (total_blocks=${CORPUS_TOTAL:-unknown} < $CORPUS_MIN)"
  else
  api "$WEBHOOK/api/query" "$KEY_PRIVATE" '{"query":"How does the Write Guard work?"}' 120
  if answered "$T"; then
  confidence=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('confidence',''))" 2>/dev/null)
  answer=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; a=json.load(sys.stdin).get('answer',''); print('nonempty' if len(a)>10 else 'empty')" 2>/dev/null)
  if [[ "$confidence" == "confident" ]] && [[ "$answer" == "nonempty" ]]; then
    pass "$T (confidence=$confidence)"
  else
    fail "$T" "expected confidence=confident + nonempty answer, got confidence=$confidence answer=$answer"
  fi
  fi
  fi

  # T15 AGENT_NEGATIVE — a rejection is an answer that says so; a request
  # without an answer is no rejection.
  T="T15 AGENT_NEGATIVE"
  api "$WEBHOOK/api/query" "$KEY_PRIVATE" '{"query":"Rezept fuer Kartoffelsuppe"}' 120
  if answered "$T"; then
  answer=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('answer',''))" 2>/dev/null)
  confidence=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('confidence',''))" 2>/dev/null)
  # The test goal is "no hallucinated answer". Accepted shapes: an explicit
  # refusal string, or a non-confident classification. "low_confidence" is the
  # actual ClassifyConfidence constant (was never "low"); with the rerank blend
  # default-on a no-hit query can land just above ScoreThreshold and classify
  # as low_confidence instead of no_relevant_blocks_found — still a rejection.
  if echo "$answer" | grep -qi "no_relevant_blocks_found\|keine antwort\|keine relevanten\|nicht relevant\|no relevant\|cannot answer\|kann.*nicht\|don't know\|weiss.*nicht\|weiß.*nicht"; then
    pass "$T (negative correctly detected)"
  elif [[ "$confidence" == "none" ]] || [[ "$confidence" == "low_confidence" ]] || [[ "$confidence" == "no_relevant_blocks_found" ]]; then
    pass "$T (confidence=$confidence)"
  else
    fail "$T" "expected rejection, got confidence=$confidence answer=${answer:0:80}"
  fi
  fi

  # T16 AGENT_BILINGUAL
  T="T16 AGENT_BILINGUAL"
  api "$WEBHOOK/api/query" "$KEY_PRIVATE" '{"query":"PostgreSQL Mount-Pfad"}' 120
  if answered "$T"; then
  answer=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('answer','').lower())" 2>/dev/null)
  if echo "$answer" | grep -qi "postgresql\|mount\|/var/lib"; then
    pass "$T"
  else
    fail "$T" "expected answer containing postgresql/mount, got: ${answer:0:80}"
  fi
  fi

  # T17 MCP_TOOLS_LIST — /mcp endpoint returns all 17 registered tools
  # (query/store/search/get/recent/update/confirm in mcp.go — update+confirm
  # kamen mit F6-C6 Stage-then-Confirm, 6b7d63e — + W12 issue_create/
  # issue_comment/issue_state aus mcp_issues.go, fa6c806 — + guard W3
  # guard_list/guard_resolve aus mcp_guard.go — + W02-8 blob_store/blob_fetch
  # aus mcp_blob.go, + W02-10 blob_link ebenda, + #201 archive aus
  # mcp_archive.go, + #211 store_batch aus mcp_store_batch.go; Registrierung ist
  # unconditional, der Count ist
  # key-unabhängig). Dieselbe Zahl steht in mcp_blob_tools_test.go: ein Tool
  # ohne Nachzug faellt dort schon im -short-Lauf auf, nicht erst hier.
  # Added after Session 24 bug: noOutput struct{} triggered outputSchema generation,
  # making clients show empty {} instead of Content[].text. Fixed in v0.27.1.
  # Both /mcp requests: a JSON-RPC result without isError is the answer (a
  # tool that failed answers 200 with isError:true), bounded by MCP_TIMEOUT_S.
  T="T17 MCP_TOOLS_LIST"
  eval_api_request POST "$WEBHOOK/mcp" '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' "$MCP_TIMEOUT_S" \
    "Authorization: Bearer $KEY_PRIVATE" jsonrpc
  if answered "$T"; then
  tool_count=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d.get('result',{}).get('tools',[])))" 2>/dev/null)
  if [[ "$tool_count" == "17" ]]; then
    pass "$T (tools=$tool_count)"
  else
    fail "$T" "expected 17 tools, got: $tool_count (resp: ${API_BODY:0:200})"
  fi
  fi

  # T18 MCP_TOOL_CALL — recent tool returns actual text content (not empty {}).
  # Guards against regression of v0.27.1 MCP structuredContent bug.
  T="T18 MCP_TOOL_CALL"
  eval_api_request POST "$WEBHOOK/mcp" \
    '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"recent","arguments":{"limit":1}}}' "$MCP_TIMEOUT_S" \
    "Authorization: Bearer $KEY_PRIVATE" jsonrpc
  if answered "$T"; then
  content_text=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; d=json.load(sys.stdin); c=d.get('result',{}).get('content',[]); print(c[0].get('text','') if c else '')" 2>/dev/null)
  if [[ -n "$content_text" ]] && [[ "$content_text" != "{}" ]]; then
    pass "$T (got content, ${#content_text} chars)"
  else
    fail "$T" "expected non-empty content, got: '${content_text:0:100}' (resp: ${API_BODY:0:200})"
  fi
  fi

  # T23 DIGEST_TRIGGER_LLMLOG (F3-P4/G28) — the manual daily-synthesis
  # trigger routes through Chain("digest", internal): its llmlog row carries
  # the ANSWERING backend's provenance (backend_name, pre-pool code logged
  # host=primary even on fallback) and required_sensitivity=internal (E6).
  # llmlog writes are async — poll briefly after the 200.
  T="T23 DIGEST_TRIGGER_LLMLOG"
  if ! $CORPUS_OK; then
    skip "$T" "corpus-bound (total_blocks=${CORPUS_TOTAL:-unknown} < $CORPUS_MIN)"
  else
  # The daily synthesis answers ok:true, not success:true.
  eval_api_request POST "$WEBHOOK/api/synthesize/daily" '{}' 200 "X-Context-Key: $KEY_PRIVATE" ok
  t23_block=""
  [[ -z "$API_NO_ANSWER" ]] && t23_block=$(printf '%s' "$API_BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('block_id',''))" 2>/dev/null)
  if [[ -n "$API_NO_ANSWER" ]]; then
    fail "$T" "no answer: $API_DETAIL ${API_BODY:0:150}"
  elif [[ -z "$t23_block" ]]; then
    fail "$T" "trigger answered without a block_id (no_activity?): ${API_BODY:0:200}"
  else
    t23_row=""
    for _ in 1 2 3 4 5; do
      t23_row=$($DB_CMD -c "SELECT COALESCE(backend_name,'')||'|'||COALESCE(required_sensitivity,'') FROM context_llm_log WHERE pipeline='dream-daily-synthesis' AND created_at > now() - interval '5 minutes' ORDER BY created_at DESC LIMIT 1" 2>/dev/null)
      [[ -n "$t23_row" && "$t23_row" != "|" ]] && break
      sleep 2
    done
    t23_backend="${t23_row%%|*}"
    t23_sens="${t23_row##*|}"
    if [[ -z "$t23_backend" ]]; then
      fail "$T" "llmlog row missing backend_name (row='$t23_row')"
    elif [[ "$t23_sens" != "internal" ]]; then
      fail "$T" "required_sensitivity expected 'internal' (E6), got '$t23_sens'"
    else
      pass "$T (block=$t23_block via backend=$t23_backend, required=internal)"
    fi
  fi
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
if (( SKIP > 0 )); then
  echo "=== Results: $PASS/$TOTAL passed, $FAIL failed, $SKIP skipped ==="
else
  echo "=== Results: $PASS/$TOTAL passed, $FAIL failed ==="
fi

if (( FAIL > 0 )); then
  exit 1
else
  exit 0
fi
