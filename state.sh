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

DB_CMD="docker exec -e PGPASSWORD=${CONTEXT_DB_PASSWORD:?} n8n-db-1 psql -U ${CONTEXT_DB_USER:-context_user} -d ${CONTEXT_DB:-context_store} -t -A"

# --- Database ---
block_active=$($DB_CMD -c "SELECT count(*) FROM context_blocks WHERE NOT is_archived;")
block_archived=$($DB_CMD -c "SELECT count(*) FROM context_blocks WHERE is_archived;")
table_count=$($DB_CMD -c "SELECT count(*) FROM information_schema.tables WHERE table_schema='public';")
table_names=$($DB_CMD -c "SELECT string_agg(table_name, ', ' ORDER BY table_name) FROM information_schema.tables WHERE table_schema='public';")
col_count=$($DB_CMD -c "SELECT count(*) FROM information_schema.columns WHERE table_name='context_blocks' AND table_schema='public';")
categories=$($DB_CMD -c "SELECT string_agg(category, ', ' ORDER BY category) FROM (SELECT DISTINCT category FROM context_blocks WHERE NOT is_archived) t;")
blob_count=$($DB_CMD -c "SELECT count(*) FROM context_blobs;")
api_keys=$($DB_CMD -c "SELECT string_agg(label || ' (' || home_scope || ')', ', ' ORDER BY label) FROM context_api_keys;")

# --- Migrations ---
migration_count=$(ls "$SCRIPT_DIR"/go/migrations/*.sql 2>/dev/null | wc -l)
migration_last=$(ls "$SCRIPT_DIR"/go/migrations/*.sql 2>/dev/null | sort | tail -1 | xargs basename 2>/dev/null || echo "none")

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
db_status=$(docker inspect --format='{{.State.Health.Status}}' n8n-db-1 2>/dev/null || echo "not running")

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
echo ""
echo "--- Migrations ---"
echo "  Count:              $migration_count"
echo "  Latest:             $migration_last"
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
echo "  n8n-db-1:           $db_status"
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
