#!/usr/bin/env bash
#
# ctx — System State Snapshot
# Usage: ./state.sh
#
# Evaluates live system state from real data sources.
# Run at session start to verify handover accuracy.
#
# ctx — Your AI's save game. By GottZ (github.com/GottZ/ctx/graphs/contributors)

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${SCRIPT_DIR}/.env"

if [[ ! -f "$ENV_FILE" ]]; then
    echo "[FATAL] .env not found at $ENV_FILE"
    exit 1
fi
set -a; source "$ENV_FILE"; set +a

# DB container resolution: env → compose → literal (issue #19).
# if-form, not `[[ ]] &&` — this script runs under set -e.
DB_CONTAINER="${CTX_DB_CONTAINER:-}"
if [[ -z "$DB_CONTAINER" ]]; then
  DB_CONTAINER="$(docker compose -f "$SCRIPT_DIR/docker-compose.yml" ps -q db 2>/dev/null | head -1)"
fi
if [[ -z "$DB_CONTAINER" ]]; then
  DB_CONTAINER="n8n-db-1"
fi

DB_CMD="docker exec -e PGPASSWORD=${CONTEXT_DB_PASSWORD:?} ${DB_CONTAINER} psql -U ${CONTEXT_DB_USER:-context_user} -d ${CONTEXT_DB:-context_store} -t -A"

# --- Database ---
block_active=$($DB_CMD -c "SELECT count(*) FROM context_blocks WHERE NOT is_archived;")
block_archived=$($DB_CMD -c "SELECT count(*) FROM context_blocks WHERE is_archived;")
table_count=$($DB_CMD -c "SELECT count(*) FROM information_schema.tables WHERE table_schema='public';")
table_names=$($DB_CMD -c "SELECT string_agg(table_name, ', ' ORDER BY table_name) FROM information_schema.tables WHERE table_schema='public';")
col_count=$($DB_CMD -c "SELECT count(*) FROM information_schema.columns WHERE table_name='context_blocks' AND table_schema='public';")
categories=$($DB_CMD -c "SELECT string_agg(category, ', ' ORDER BY category) FROM (SELECT DISTINCT category FROM context_blocks WHERE NOT is_archived) t;")
blob_count=$($DB_CMD -c "SELECT count(*) FROM context_blobs;")
api_keys=$($DB_CMD -c "SELECT string_agg(label || ' (' || home_scope || ')', ', ' ORDER BY label) FROM context_api_keys;")
# recall_check last valid run (Achse 01, Mig 108). n/a on pre-108 DBs — the
# table-missing psql error is swallowed by 2>/dev/null + the || fallback, so
# state.sh never aborts under set -e on an older schema (design/01 §4.4).
recall_last=$($DB_CMD -c "SELECT to_char(max(ran_at),'YYYY-MM-DD')||' avg='||coalesce(round(avg(recall_avg)::numeric,3)::text,'n/a') FROM context_recall_runs WHERE valid AND run_group=(SELECT run_group FROM context_recall_runs WHERE valid ORDER BY ran_at DESC LIMIT 1);" 2>/dev/null || echo "n/a")
recall_last=${recall_last:-n/a}

# Re-Embed-Migration (Achse 04, Mig 114) + geparkte infinity-Mengen BEIDER Pfade
# (design/04 §4.4/§4.7 W04-7). n/a auf Vor-114-Schemas — der table-missing
# psql-Fehler wird von 2>/dev/null + || geschluckt, set -e-sicheres Bestands-
# muster wie die Recall-Zeile darüber. Kein vorgetäuschter Wert: fehlt die
# Tabelle, bleibt die Zeile schlicht "n/a".
embed_migration_active=$($DB_CMD -c "SELECT status||' '||from_model||'→'||to_model||' migrated='||migrated_count||'/'||total_blocks||' failed='||failed_count||' skipped='||skipped_count FROM context_embed_migrations WHERE status IN ('pending','running','paused','verifying') LIMIT 1;" 2>/dev/null || echo "n/a")
embed_migration_active=${embed_migration_active:-none}
# infinity-Menge migration-scoped (parked oversize/sensitivity der laufenden
# Migration) — nur sinnvoll bei aktiver Migration, sonst 0.
embed_inf_migration=$($DB_CMD -c "SELECT count(*) FROM context_embed_failures WHERE migration_id IS NOT NULL AND next_attempt_at='infinity';" 2>/dev/null || echo "n/a")
embed_inf_migration=${embed_inf_migration:-n/a}
# infinity-Menge backfill-scoped (Pfad A/B oversize — existiert unabhängig von
# jeder Migration; §4.4: dauerhaft geparkte Blöcke, Operator-sichtbar statt still).
embed_inf_backfill=$($DB_CMD -c "SELECT count(*) FROM context_embed_failures WHERE migration_id IS NULL AND next_attempt_at='infinity';" 2>/dev/null || echo "n/a")
embed_inf_backfill=${embed_inf_backfill:-n/a}

# --- Migrations ---
migration_count=$(ls "$SCRIPT_DIR"/go/migrations/*.sql 2>/dev/null | wc -l)
migration_last=$(ls "$SCRIPT_DIR"/go/migrations/*.sql 2>/dev/null | sort | tail -1 | xargs basename 2>/dev/null || echo "none")

# _migrations-DB-Abgleich (read-only psql-Kurzcheck, funktioniert auch bei
# totem Daemon — design/03 §4.8). Repo-Dateistand vs. Live-Tabellenstand.
migration_last_num=$(echo "$migration_last" | grep -oP '^\d+' || echo "0")
db_migration_count=$($DB_CMD -c "SELECT count(*) FROM _migrations;" 2>/dev/null || echo "?")
db_migration_max=$($DB_CMD -c "SELECT max(version) FROM _migrations;" 2>/dev/null || echo "?")
if [[ "$migration_last_num" =~ ^[0-9]+$ && "$db_migration_max" =~ ^[0-9]+$ ]]; then
  migration_pending=$((migration_last_num - db_migration_max))
  if [[ "$migration_pending" -eq 0 ]]; then
    migration_sync="in sync"
  else
    migration_sync="${migration_pending} PENDING"
  fi
else
  migration_sync="unbekannt (DB-Abfrage fehlgeschlagen)"
fi

# checksums_missing (bedingt, Forward-Kompatibilität — design/03 §4.8: die
# Spalte _migrations.checksum existiert erst nach dem 108–112-Deploy. Ohne
# sie KEIN vorgetäuschter Wert, die Zeile bleibt schlicht weg statt "0" zu
# lügen; set -e-sicheres Bestandsmuster wie die Zeilen darüber).
checksum_col_exists=$($DB_CMD -c "SELECT count(*) FROM information_schema.columns WHERE table_name='_migrations' AND column_name='checksum';" 2>/dev/null || echo "0")
checksums_missing_suffix=""
if [[ "$checksum_col_exists" == "1" ]]; then
  checksums_missing=$($DB_CMD -c "SELECT count(*) FROM _migrations WHERE checksum IS NULL;" 2>/dev/null || echo "?")
  checksums_missing_suffix=" checksums_missing=${checksums_missing}"
fi

# --- Contract (Schema-Contract-Check via `ctx contract`, W03-4 CLI) ---
# set -e-sicher: der if/else fängt den Exit-Code ab, BEVOR eine Pipe/Zuweisung
# ihn verschlucken könnte (bekannter Projekt-Quirk: "EXIT nach Pipe misst die
# Pipe"). /api/contract ist admin-gated — ohne Admin-Key antwortet der Server
# 403 und die CLI Exit 3 (Befund 2026-07-25, gleiche Lücke wie test.sh T07);
# CTX_KEY aus der Env schlägt den Datei-Key (internal/cli.LoadConfig).
if contract_out=$(CTX_KEY="${CTX_ADMIN_KEY:-}" ctx contract 2>&1); then
  contract_rc=0
else
  contract_rc=$?
fi
contract_detail=""
case "$contract_rc" in
  0)
    contract_line="ok"
    ;;
  1)
    if [[ "$contract_out" == *"unknown command"* ]]; then
      contract_line="nicht verfügbar (installierte CLI kennt \"contract\" nicht — Exit 1/unknown command, Vor-W03-4-Stand)"
    else
      contract_line="DRIFT"
      contract_detail=$(echo "$contract_out" | head -5)
    fi
    ;;
  2)
    contract_line="UNCHECKED"
    ;;
  3)
    contract_line="nicht verfügbar (CLI/Endpoint, Exit 3)"
    ;;
  *)
    contract_line="nicht verfügbar (unerwarteter Exit $contract_rc)"
    ;;
esac

# --- Go ---
go_version=$(grep '^go ' "$SCRIPT_DIR/go/go.mod" | awk '{print $2}')
go_deps_direct=$(grep -Pv '// indirect' "$SCRIPT_DIR/go/go.mod" | grep -Pc '^\t' 2>/dev/null || echo "0")
go_deps_indirect=$(grep -Pc '// indirect' "$SCRIPT_DIR/go/go.mod" 2>/dev/null || echo "0")
go_packages=$(ls -d "$SCRIPT_DIR"/go/internal/*/ 2>/dev/null | wc -l)
go_test_funcs=$(grep -r 'func Test' "$SCRIPT_DIR/go/" --include='*.go' 2>/dev/null | wc -l)
go_binaries=$(ls "$SCRIPT_DIR"/go/cmd/ 2>/dev/null | tr '\n' ', ' | sed 's/,$//')

# --- CLI ---
cli_register=$(grep -c 'root\.AddCommand' "$SCRIPT_DIR/go/internal/cli/commands.go" 2>/dev/null || echo 0)
cli_main=$(grep -c 'AddCommand' "$SCRIPT_DIR/go/cmd/ctx/main.go" 2>/dev/null || echo 0)
cli_commands=$((cli_register + cli_main))

# --- Containers ---
ctx_status=$(docker inspect --format='{{.State.Health.Status}}' ctx 2>/dev/null || echo "not running")
db_status=$(docker inspect --format='{{.State.Health.Status}}' "$DB_CONTAINER" 2>/dev/null || echo "not running")

# --- Config ---
embed_model="${OLLAMA_EMBED_MODEL:-not set}"
chat_model="${OLLAMA_CHAT_MODEL:-not set}"
embed_dims="${OLLAMA_EMBED_DIMS:-not set}"

# --- Git ---
git_branch=$(git -C "$SCRIPT_DIR" branch --show-current 2>/dev/null || echo "?")
git_commit=$(git -C "$SCRIPT_DIR" log --oneline -1 2>/dev/null || echo "?")
git_dirty=$(git -C "$SCRIPT_DIR" status --porcelain 2>/dev/null | wc -l)
# Agent-worktree setup keeps resetting this to the empty .git/hooks dir,
# silently disabling pre-commit/commit-msg/pre-push repo-wide. Surface it.
git_hookspath=$(git -C "$SCRIPT_DIR" config core.hooksPath 2>/dev/null || echo "unset")
if [ "$git_hookspath" = ".hooks" ]; then
  git_hookspath=".hooks (ok)"
else
  git_hookspath="$git_hookspath — DRIFT, fix: git config core.hooksPath .hooks"
fi

# --- Backup ---
latest_backup=$(ls -t "$SCRIPT_DIR"/backups/*.dump 2>/dev/null | head -1 | xargs basename 2>/dev/null || echo "none")
backup_timer=$(systemctl is-active n8n-backup.timer 2>/dev/null || echo "unknown")

# --- Output ---
echo "=== ctx System State ==="
echo "Date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo ""
echo "--- Database ---"
echo "  Blocks (active):    $block_active"
echo "  Blocks (archived):  $block_archived"
echo "  Tables:             $table_count ($table_names)"
echo "  Columns (blocks):   $col_count"
echo "  Categories:         $categories"
echo "  Blobs:              $blob_count"
echo "  API Keys:           $api_keys"
echo "  Recall (last valid):$recall_last"
echo "  Re-Embed migration: $embed_migration_active"
echo "  Parked ∞ (mig/bkfl): $embed_inf_migration / $embed_inf_backfill"
echo ""
echo "--- Migrations ---"
echo "  Count:              $migration_count"
echo "  Latest:             $migration_last"
echo "  DB-Abgleich:        repo $migration_count files (max $migration_last_num) / live $db_migration_count rows (max $db_migration_max) — $migration_sync${checksums_missing_suffix}"
echo ""
echo "--- Contract ---"
echo "  contract:           $contract_line"
if [[ -n "$contract_detail" ]]; then
  echo "$contract_detail" | sed 's/^/                      /'
fi
echo ""
echo "--- Go ---"
echo "  Version:            $go_version"
echo "  Deps:               $go_deps_direct direct, $go_deps_indirect indirect"
echo "  Packages:           $go_packages"
echo "  Test functions:     $go_test_funcs"
echo "  Binaries:           $go_binaries"
echo "  CLI commands:       $cli_commands"
# Inventory, not validation: the multi-tenant wave starts from this grep
# (masterplan E8 convention — every wave marks tenant-relevant spots).
mt_markers=$(grep -rn "TODO(multi-tenant)" "$SCRIPT_DIR/go" 2>/dev/null | wc -l)
echo "  TODO(multi-tenant): $mt_markers markers"
echo ""
echo "--- Containers ---"
echo "  ctx:                $ctx_status"
echo "  db:                 $db_status ($DB_CONTAINER)"
echo ""
echo "--- Config ---"
echo "  Embed model:        $embed_model"
echo "  Chat model:         $chat_model"
echo "  Embed dims:         $embed_dims"
echo ""
echo "--- Git ---"
echo "  Branch:             $git_branch"
echo "  HEAD:               $git_commit"
echo "  Dirty files:        $git_dirty"
echo "  hooksPath:          $git_hookspath"
echo ""
echo "--- Backup ---"
echo "  Latest:             $latest_backup"
echo "  Timer:              $backup_timer"
