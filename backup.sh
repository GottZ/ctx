#!/usr/bin/env bash
# =============================================================================
# n8n + Context Store — Automated PostgreSQL Backup Script
# Runs pg_dump via docker exec against n8n-db-1 container
# Stores backups in /compose/n8n/backups/ with 7-day rotation
# =============================================================================
# Part of ctx by GottZ (github.com/GottZ/ctx/graphs/contributors)
# Source: https://github.com/GottZ/ctx
#
# Extension points (site-local, NOT tracked — see .gitignore `backup.d/`):
#   Every `backup.d/*.sh` is sourced after the core dumps, in name order, with
#   these helpers in scope:
#     backup_dump   LABEL USER PASSWORD DB [pg_dump-args...]
#                   → dumps DB as LABEL-<DATE>.dump, registers it for the
#                     integrity check, counts a failure into ERRORS
#     backup_rotate GLOB DAYS
#                   → registers an extra rotation rule (evaluated in step 4)
#     backup_require_var NAME...
#                   → [FATAL] exit 1 when a variable is unset/empty
#   The core script keeps the public shape (context_store + n8n); everything
#   deployment-specific (extra databases, table exclusions, shorter retention)
#   lives in the hooks. A hook that fails does not abort the run — it counts
#   as an error in the summary.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BACKUP_DIR="${SCRIPT_DIR}/backups"
ENV_FILE="${SCRIPT_DIR}/.env"
HOOKS_DIR="${BACKUP_HOOKS_DIR:-${SCRIPT_DIR}/backup.d}"
CONTAINER="${CTX_DB_CONTAINER:-n8n-db-1}"
RETENTION_DAYS=7
DATE=$(date +%Y%m%d-%H%M%S)

# ---------------------------------------------------------------------------
# Load credentials from .env
# ---------------------------------------------------------------------------
if [[ ! -f "$ENV_FILE" ]]; then
    echo "[FATAL] $(date -Iseconds) — .env not found at $ENV_FILE"
    exit 1
fi

# Source .env (handles duplicate keys — last wins)
set -a
source "$ENV_FILE"
set +a

# ---------------------------------------------------------------------------
# Helpers (also the hook API — keep signatures stable)
# ---------------------------------------------------------------------------
ERRORS=0
DUMP_FILES=()
ROTATIONS=()   # entries "GLOB|DAYS"

backup_require_var() {
    local var
    for var in "$@"; do
        if [[ -z "${!var:-}" ]]; then
            echo "[FATAL] $(date -Iseconds) — Missing required variable: $var"
            exit 1
        fi
    done
}

# backup_dump LABEL USER PASSWORD DB [pg_dump-args...]
backup_dump() {
    local label="$1" user="$2" pass="$3" db="$4"
    shift 4
    local file="${BACKUP_DIR}/${label}-${DATE}.dump"
    echo ""
    echo "[STEP]  Dumping ${db} database (${label})..."
    if docker exec -e PGPASSWORD="$pass" "$CONTAINER" \
        pg_dump -U "$user" -d "$db" -Fc "$@" \
        > "$file" 2>/dev/null; then
        echo "[OK]    ${label} → $file ($(du -h "$file" | cut -f1))"
        DUMP_FILES+=("$file")
    else
        echo "[ERROR] ${label} dump failed!"
        ERRORS=$((ERRORS + 1))
    fi
}

# backup_rotate GLOB DAYS — extra rotation rule, evaluated in step 4
backup_rotate() {
    ROTATIONS+=("$1|$2")
}

# Validate required vars
backup_require_var POSTGRES_USER POSTGRES_PASSWORD CONTEXT_DB CONTEXT_DB_USER CONTEXT_DB_PASSWORD

# n8n DB uses the non-root user if available, else admin
N8N_DB="n8n"
N8N_USER="${POSTGRES_NON_ROOT_USER:-n8n}"
N8N_PASS="${POSTGRES_NON_ROOT_PASSWORD:-$POSTGRES_PASSWORD}"

mkdir -p "$BACKUP_DIR"
umask 077

echo "==========================================================================="
echo "[INFO]  $(date -Iseconds) — Backup started"
echo "[INFO]  Container: $CONTAINER"
echo "[INFO]  Target dir: $BACKUP_DIR"
echo "[INFO]  Hooks dir:  $HOOKS_DIR"
echo "==========================================================================="

# ---------------------------------------------------------------------------
# 1. Backup context_store database
# ---------------------------------------------------------------------------
backup_dump context_store "$CONTEXT_DB_USER" "$CONTEXT_DB_PASSWORD" "$CONTEXT_DB"

# ---------------------------------------------------------------------------
# 2. Backup n8n database
# ---------------------------------------------------------------------------
backup_dump n8n "$N8N_USER" "$N8N_PASS" "$N8N_DB"

# ---------------------------------------------------------------------------
# 2b. Site-local hooks (backup.d/*.sh) — extra dumps + rotation rules
# ---------------------------------------------------------------------------
if [[ -d "$HOOKS_DIR" ]]; then
    for hook in "$HOOKS_DIR"/*.sh; do
        [[ -f "$hook" ]] || continue
        echo ""
        echo "[HOOK]  $(basename "$hook")"
        # `|| …` keeps set -e from aborting the whole run on a hook error;
        # backup_dump does its own error accounting, anything else counts once.
        # shellcheck disable=SC1090
        source "$hook" || { echo "[ERROR] hook $(basename "$hook") failed"; ERRORS=$((ERRORS + 1)); }
    done
fi

# ---------------------------------------------------------------------------
# 3. Integrity check — pg_restore --list on every dump
# ---------------------------------------------------------------------------
echo ""
echo "[STEP]  Running integrity checks (pg_restore --list)..."

for dump_file in "${DUMP_FILES[@]}"; do
    if [[ ! -f "$dump_file" || ! -s "$dump_file" ]]; then
        echo "[WARN]  Skipping integrity check for missing/empty: $dump_file"
        ERRORS=$((ERRORS + 1))
        continue
    fi

    BASENAME=$(basename "$dump_file")
    TOC_COUNT=$(docker exec -i "$CONTAINER" pg_restore --list < "$dump_file" 2>/dev/null | wc -l)

    if [[ "$TOC_COUNT" -gt 0 ]]; then
        echo "[OK]    $BASENAME — TOC entries: $TOC_COUNT"
    else
        echo "[ERROR] $BASENAME — integrity check failed (0 TOC entries)!"
        ERRORS=$((ERRORS + 1))
    fi
done

# ---------------------------------------------------------------------------
# 4. Rotation — core dumps after RETENTION_DAYS, hook rules with their own age.
#    Patterns are explicit (no `*.dump`) so a hook's files never fall under
#    the core rule and vice versa.
# ---------------------------------------------------------------------------
echo ""
echo "[STEP]  Rotating backups (core: ${RETENTION_DAYS} days; hook rules: ${#ROTATIONS[@]})..."

DELETED=0
rotate_glob() {
    local glob="$1" days="$2" old_file
    while IFS= read -r -d '' old_file; do
        echo "[DEL]   $(basename "$old_file") (${glob}, ${days}d)"
        rm -f "$old_file"
        DELETED=$((DELETED + 1))
    done < <(find "$BACKUP_DIR" -maxdepth 1 -name "$glob" -mtime +"${days}" -print0 2>/dev/null)
}
rotate_glob "context_store-*.dump" "$RETENTION_DAYS"
rotate_glob "n8n-*.dump" "$RETENTION_DAYS"
for rule in "${ROTATIONS[@]}"; do
    rotate_glob "${rule%%|*}" "${rule##*|}"
done

echo "[INFO]  Deleted $DELETED old backup(s)"

# ---------------------------------------------------------------------------
# 5. Summary
# ---------------------------------------------------------------------------
echo ""
echo "==========================================================================="
echo "[INFO]  $(date -Iseconds) — Backup finished"
echo "[INFO]  Current backups in $BACKUP_DIR:"
ls -lh "$BACKUP_DIR"/*.dump 2>/dev/null || echo "         (none)"
echo ""

TOTAL_SIZE=$(du -sh "$BACKUP_DIR" | cut -f1)
echo "[INFO]  Total backup size: $TOTAL_SIZE"

if [[ $ERRORS -gt 0 ]]; then
    echo "[WARN]  Completed with $ERRORS error(s)!"
    exit 1
else
    echo "[OK]    All backups completed successfully"
    exit 0
fi
