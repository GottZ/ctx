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
# fällt T07 auf exakt die alte Zähl-Probe zurück (46 Tabellen / 40 Spalten
# context_blocks) — dieselbe Prüfstärke wie vor dieser Welle, nicht
# schwächer. Das Upgrade auf den generierten Contract greift automatisch,
# sobald die deployte CLI "contract" kennt — kein Script-Change nötig.
T="T07 SCHEMA_INTEGRITY"

t07_count_fallback() {
  local reason="$1"
  local tc col
  tc=$($DB_CMD -c "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name NOT LIKE '%_snapshot_%';" 2>/dev/null | tr -d '[:space:]')
  col=$($DB_CMD -c "SELECT count(*) FROM information_schema.columns WHERE table_name='context_blocks';" 2>/dev/null | tr -d '[:space:]')
  if [[ "$tc" == "46" ]] && [[ "$col" == "40" ]]; then
    pass "$T (contract-CLI nicht verfügbar — Zähl-Fallback (Prä-Deploy); tables=$tc, columns=$col; $reason)"
  else
    fail "$T" "Zähl-Fallback (Prä-Deploy, $reason): expected 46 tables + 40 columns, got tables=$tc columns=$col"
  fi
}

# set -e-sicher: der if/else fängt den Exit-Code ab, BEVOR eine Pipe/Zuweisung
# ihn verschlucken könnte (Projekt-Quirk "EXIT nach Pipe misst die Pipe";
# dasselbe Muster wie state.sh's Contract-Zeile, W03-5).
if t07_out=$(ctx contract 2>&1); then
  t07_rc=0
else
  t07_rc=$?
fi

case "$t07_rc" in
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

# T19 GRAPH_EGO — GET /api/graph/ego (G07/F5-W1). Four assertions per the
# wave gate: node/edge counts > 0, NO content field anywhere in the payload,
# stats.truncated present, and 404-equality (invisible private block vs
# nonexistent UUID must answer byte-identically — no existence oracle).
T="T19 GRAPH_EGO"
# Focus: the highest-degree non-archived private/shared block (visible to
# KEY_PRIVATE, guaranteed >= 1 edge so the edge-count assertion is meaningful).
t19_focus=$($DB_CMD -c "SELECT b.id FROM context_blocks b
  JOIN context_dream_links l ON l.source_block_id = b.id OR l.target_block_id = b.id
  WHERE NOT b.is_archived AND b.type_name <> 'system-meta' AND b.scope IN ('private','shared')
  GROUP BY b.id ORDER BY count(*) DESC LIMIT 1;" 2>/dev/null | tr -d '[:space:]')
if [[ -z "$t19_focus" ]]; then
  fail "$T" "no linked block found to use as focus"
else
  resp=$(curl -s --max-time 15 "$WEBHOOK/api/graph/ego?block=$t19_focus&hops=2" \
    -H "X-Context-Key: $KEY_PRIVATE" 2>/dev/null)
  t19_check=$(echo "$resp" | python3 -c "
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
    fail "$T" "payload check: ${t19_check:-unparseable response: ${resp:0:100}}"
  else
    # 404-equality: a private block (invisible to KEY_WORK) and a nonexistent
    # UUID must produce byte-identical 404 bodies.
    t19_priv=$($DB_CMD -c "SELECT id FROM context_blocks WHERE scope='private' AND NOT is_archived LIMIT 1;" 2>/dev/null | tr -d '[:space:]')
    t19_missing="00000000-0000-7000-8000-000000000000"
    t19_b1=$(curl -s --max-time 10 -w '\n%{http_code}' "$WEBHOOK/api/graph/ego?block=$t19_priv" \
      -H "X-Context-Key: $KEY_WORK" 2>/dev/null)
    t19_b2=$(curl -s --max-time 10 -w '\n%{http_code}' "$WEBHOOK/api/graph/ego?block=$t19_missing" \
      -H "X-Context-Key: $KEY_WORK" 2>/dev/null)
    t19_code1="${t19_b1##*$'\n'}"; t19_body1="${t19_b1%$'\n'*}"
    t19_code2="${t19_b2##*$'\n'}"; t19_body2="${t19_b2%$'\n'*}"
    if [[ "$t19_code1" == "404" && "$t19_code2" == "404" && "$t19_body1" == "$t19_body2" ]]; then
      pass "$T (${t19_check#OK }, 404-equality holds)"
    else
      fail "$T" "404-oracle: invisible=$t19_code1 missing=$t19_code2 bodies_equal=$([[ "$t19_body1" == "$t19_body2" ]] && echo yes || echo no)"
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
  t20_403=$(curl -s --max-time 10 -o /dev/null -w '%{http_code}' "$WEBHOOK/api/settings" \
    -H "X-Context-Key: $KEY_PRIVATE" 2>/dev/null)
  t20_cur=$(curl -s --max-time 10 "$WEBHOOK/api/settings/rerank.blend_weight" \
    -H "X-Context-Key: $CTX_ADMIN_KEY" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin)['setting']['value'])" 2>/dev/null)
  if [[ "$t20_403" != "403" ]]; then
    fail "$T" "non-admin GET expected 403, got $t20_403"
  elif [[ -z "$t20_cur" ]]; then
    fail "$T" "could not read current rerank.blend_weight via admin key"
  else
    t20_put=$(curl -s --max-time 15 -X PUT "$WEBHOOK/api/settings/rerank.blend_weight" \
      -H "Content-Type: application/json" -H "X-Context-Key: $CTX_ADMIN_KEY" \
      -d "{\"value\":$t20_cur}" 2>/dev/null)
    t20_src=$(echo "$t20_put" | python3 -c "import sys,json; print(json.load(sys.stdin).get('source',''))" 2>/dev/null)
    t20_del=$(curl -s --max-time 15 -X DELETE "$WEBHOOK/api/settings/rerank.blend_weight" \
      -H "X-Context-Key: $CTX_ADMIN_KEY" 2>/dev/null)
    t20_dsrc=$(echo "$t20_del" | python3 -c "import sys,json; print(json.load(sys.stdin).get('source',''))" 2>/dev/null)
    if [[ "$t20_src" == "db" && -n "$t20_dsrc" && "$t20_dsrc" != "db" ]]; then
      pass "$T (403 enforced, override db→$t20_dsrc, value=$t20_cur)"
    else
      fail "$T" "put_source=${t20_src:-unparseable} delete_source=${t20_dsrc:-unparseable}"
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
  t21_403=$(curl -s --max-time 10 -o /dev/null -w '%{http_code}' -X POST "$WEBHOOK/api/manage" \
    -H "Content-Type: application/json" -H "X-Context-Key: $KEY_PRIVATE" \
    -d '{"action":"backend-list"}' 2>/dev/null)
  t21_count=$(curl -s --max-time 10 -X POST "$WEBHOOK/api/manage" \
    -H "Content-Type: application/json" -H "X-Context-Key: $CTX_ADMIN_KEY" \
    -d '{"action":"backend-list"}' 2>/dev/null \
    | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d['backends']) if d.get('success') else 0)" 2>/dev/null)
  if [[ "$t21_403" != "403" ]]; then
    fail "$T" "non-admin backend-list expected 403, got $t21_403"
  elif [[ -z "$t21_count" || "$t21_count" -lt 3 ]]; then
    fail "$T" "expected >=3 bootstrapped backends, got ${t21_count:-unparseable}"
  else
    pass "$T (403 enforced, $t21_count pool rows)"
  fi
fi

# T22 BLOCK_SENSITIVITY (F3-P3/M055) — store with explicit sensitivity stamps
# source=manual, an unconfirmed downgrade is rejected (the confirm-gated
# direction), a confirmed one applies + audits. No LLM: pure write surface.
T="T22 BLOCK_SENSITIVITY"
t22_block=$(api "$WEBHOOK/api/store" "$KEY_PRIVATE" \
  '{"category":"reference","title":"__t22_sensitivity_probe","content":"sensitivity gate probe '"$(date +%s)"'","sensitivity":"personal"}' 15)
t22_id=$(echo "$t22_block" | python3 -c "import sys,json; d=json.load(sys.stdin); b=d.get('block') or {}; print(b.get('id','') if d.get('success') else '')" 2>/dev/null)
t22_sens=$(echo "$t22_block" | python3 -c "import sys,json; b=json.load(sys.stdin).get('block') or {}; print(b.get('sensitivity',''), b.get('sensitivity_source',''))" 2>/dev/null)
if [[ -z "$t22_id" ]]; then
  fail "$T" "store with sensitivity failed: $(echo "$t22_block" | head -c 200)"
elif [[ "$t22_sens" != "personal manual" ]]; then
  fail "$T" "expected sensitivity='personal manual', got '$t22_sens'"
else
  t22_deny=$(api "$WEBHOOK/api/manage" "$KEY_PRIVATE" \
    '{"action":"update","id":"'"$t22_id"'","data":{"sensitivity":"public"}}' 15)
  t22_deny_ok=$(echo "$t22_deny" | python3 -c "import sys,json; d=json.load(sys.stdin); print('denied' if not d.get('success') and 'confirm_sensitivity_downgrade' in d.get('error','') else 'leaked')" 2>/dev/null)
  t22_conf=$(api "$WEBHOOK/api/manage" "$KEY_PRIVATE" \
    '{"action":"update","id":"'"$t22_id"'","data":{"sensitivity":"public","confirm_sensitivity_downgrade":true}}' 15)
  t22_conf_state=$(echo "$t22_conf" | python3 -c "import sys,json; d=json.load(sys.stdin); b=d.get('block') or {}; a=(b.get('metadata') or {}).get('sensitivity_audit') or {}; print(b.get('sensitivity',''), 'audited' if a.get('from')=='personal' and a.get('to')=='public' else 'unaudited')" 2>/dev/null)
  api "$WEBHOOK/api/manage" "$KEY_PRIVATE" '{"action":"delete","id":"'"$t22_id"'"}' 15 >/dev/null 2>&1
  if [[ "$t22_deny_ok" != "denied" ]]; then
    fail "$T" "unconfirmed downgrade not rejected: $(echo "$t22_deny" | head -c 200)"
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
  t24_403=$(curl -s --max-time 10 -o /dev/null -w '%{http_code}' -X POST "$WEBHOOK/api/manage" \
    -H "Content-Type: application/json" -H "X-Context-Key: $KEY_PRIVATE" \
    -d '{"action":"blocks-audit-status"}' 2>/dev/null)
  t24_status=$(curl -s --max-time 10 -X POST "$WEBHOOK/api/manage" \
    -H "Content-Type: application/json" -H "X-Context-Key: $CTX_ADMIN_KEY" \
    -d '{"action":"blocks-audit-status"}' 2>/dev/null \
    | python3 -c "import sys,json; d=json.load(sys.stdin); print('ok' if d.get('success') and 'pending' in d and isinstance(d.get('by_source'),dict) else 'bad')" 2>/dev/null)
  t24_extern=$(curl -s --max-time 10 -X POST "$WEBHOOK/api/manage" \
    -H "Content-Type: application/json" -H "X-Context-Key: $CTX_ADMIN_KEY" \
    -d '{"action":"backend-create","data":{"name":"__t24_classify_probe","base_url":"https://api.example.com/v1","protocol":"openai","provider_class":"generic","trust":"public","locality":"external","roles":["classify"],"model_map":{"default":"m"},"confirm_trust_elevation":true,"metadata":{"embed_equivalence_verified":true}}}' 2>/dev/null)
  t24_extern_ok=$(echo "$t24_extern" | python3 -c "import sys,json; d=json.load(sys.stdin); print('blocked' if not d.get('success') and 'classify' in json.dumps(d) else 'leaked')" 2>/dev/null)
  if [[ "$t24_extern_ok" == "leaked" ]]; then
    # If the probe row was created, remove it before failing.
    t24_pid=$(echo "$t24_extern" | python3 -c "import sys,json; b=json.load(sys.stdin).get('backend') or {}; print(b.get('id',''))" 2>/dev/null)
    [[ -n "$t24_pid" ]] && curl -s --max-time 10 -X POST "$WEBHOOK/api/manage" \
      -H "Content-Type: application/json" -H "X-Context-Key: $CTX_ADMIN_KEY" \
      -d '{"action":"backend-delete","id":"'"$t24_pid"'"}' >/dev/null 2>&1
  fi
  if [[ "$t24_403" != "403" ]]; then
    fail "$T" "non-admin blocks-audit-status expected 403, got $t24_403"
  elif [[ "$t24_status" != "ok" ]]; then
    fail "$T" "admin status response malformed"
  elif [[ "$t24_extern_ok" != "blocked" ]]; then
    fail "$T" "classify+external backend-create not rejected: $(echo "$t24_extern" | head -c 200)"
  else
    pass "$T (403 enforced, status shape, classify hard-local 422)"
  fi
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

  # T17 MCP_TOOLS_LIST — /mcp endpoint returns all 10 registered tools
  # (query/store/search/get/recent/update/confirm in mcp.go — update+confirm
  # kamen mit F6-C6 Stage-then-Confirm, 6b7d63e — + W12 issue_create/
  # issue_comment/issue_state aus mcp_issues.go, fa6c806; Registrierung ist
  # unconditional, der Count ist key-unabhängig).
  # Added after Session 24 bug: noOutput struct{} triggered outputSchema generation,
  # making clients show empty {} instead of Content[].text. Fixed in v0.27.1.
  T="T17 MCP_TOOLS_LIST"
  mcp_resp=$(curl -sS -X POST "$WEBHOOK/mcp" \
    -H "Authorization: Bearer $KEY_PRIVATE" \
    -H "Content-Type: application/json" \
    -H "Accept: application/json, text/event-stream" \
    -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' 2>/dev/null)
  tool_count=$(echo "$mcp_resp" | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d.get('result',{}).get('tools',[])))" 2>/dev/null)
  if [[ "$tool_count" == "10" ]]; then
    pass "$T (tools=$tool_count)"
  else
    fail "$T" "expected 10 tools, got: $tool_count (resp: ${mcp_resp:0:200})"
  fi

  # T18 MCP_TOOL_CALL — recent tool returns actual text content (not empty {}).
  # Guards against regression of v0.27.1 MCP structuredContent bug.
  T="T18 MCP_TOOL_CALL"
  mcp_call=$(curl -sS -X POST "$WEBHOOK/mcp" \
    -H "Authorization: Bearer $KEY_PRIVATE" \
    -H "Content-Type: application/json" \
    -H "Accept: application/json, text/event-stream" \
    -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"recent","arguments":{"limit":1}}}' 2>/dev/null)
  content_text=$(echo "$mcp_call" | python3 -c "import sys,json; d=json.load(sys.stdin); c=d.get('result',{}).get('content',[]); print(c[0].get('text','') if c else '')" 2>/dev/null)
  if [[ -n "$content_text" ]] && [[ "$content_text" != "{}" ]]; then
    pass "$T (got content, ${#content_text} chars)"
  else
    fail "$T" "expected non-empty content, got: '${content_text:0:100}' (resp: ${mcp_call:0:200})"
  fi

  # T23 DIGEST_TRIGGER_LLMLOG (F3-P4/G28) — the manual daily-synthesis
  # trigger routes through Chain("digest", internal): its llmlog row carries
  # the ANSWERING backend's provenance (backend_name, pre-pool code logged
  # host=primary even on fallback) and required_sensitivity=internal (E6).
  # llmlog writes are async — poll briefly after the 200.
  T="T23 DIGEST_TRIGGER_LLMLOG"
  t23_resp=$(api "$WEBHOOK/api/synthesize/daily" "$KEY_PRIVATE" '{}' 200)
  t23_block=$(echo "$t23_resp" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('block_id','') if d.get('ok') else '')" 2>/dev/null)
  if [[ -z "$t23_block" ]]; then
    fail "$T" "trigger failed or no_activity: $(echo "$t23_resp" | head -c 200)"
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
