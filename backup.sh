#!/usr/bin/env bash
# =============================================================================
# n8n + Context Store — Automated PostgreSQL Backup Script
# Runs pg_dump via docker exec against n8n-db-1 container
# Stores backups in /compose/n8n/backups/ with 7-day rotation
# =============================================================================
# Part of ctx by GottZ (github.com/GottZ/ctx/graphs/contributors)
# Source: https://github.com/GottZ/ctx
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BACKUP_DIR="${SCRIPT_DIR}/backups"
ENV_FILE="${SCRIPT_DIR}/.env"
CONTAINER="n8n-db-1"
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

# Validate required vars
for var in POSTGRES_USER POSTGRES_PASSWORD CONTEXT_DB CONTEXT_DB_USER CONTEXT_DB_PASSWORD; do
    if [[ -z "${!var:-}" ]]; then
        echo "[FATAL] $(date -Iseconds) — Missing required variable: $var"
        exit 1
    fi
done

# n8n DB uses the non-root user if available, else admin
N8N_DB="n8n"
N8N_USER="${POSTGRES_NON_ROOT_USER:-n8n}"
N8N_PASS="${POSTGRES_NON_ROOT_PASSWORD:-$POSTGRES_PASSWORD}"

mkdir -p "$BACKUP_DIR"

echo "==========================================================================="
echo "[INFO]  $(date -Iseconds) — Backup started"
echo "[INFO]  Container: $CONTAINER"
echo "[INFO]  Target dir: $BACKUP_DIR"
echo "==========================================================================="

ERRORS=0

# ---------------------------------------------------------------------------
# 1. Backup context_store database
# ---------------------------------------------------------------------------
CTX_FILE="${BACKUP_DIR}/context_store-${DATE}.dump"
echo ""
echo "[STEP]  Dumping context_store database..."

if docker exec -e PGPASSWORD="$CONTEXT_DB_PASSWORD" "$CONTAINER" \
    pg_dump -U "$CONTEXT_DB_USER" -d "$CONTEXT_DB" -Fc \
    > "$CTX_FILE" 2>/dev/null; then
    CTX_SIZE=$(du -h "$CTX_FILE" | cut -f1)
    echo "[OK]    context_store → $CTX_FILE ($CTX_SIZE)"
else
    echo "[ERROR] context_store dump failed!"
    ERRORS=$((ERRORS + 1))
fi

# ---------------------------------------------------------------------------
# 2. Backup n8n database
# ---------------------------------------------------------------------------
N8N_FILE="${BACKUP_DIR}/n8n-${DATE}.dump"
echo ""
echo "[STEP]  Dumping n8n database..."

if docker exec -e PGPASSWORD="$N8N_PASS" "$CONTAINER" \
    pg_dump -U "$N8N_USER" -d "$N8N_DB" -Fc \
    > "$N8N_FILE" 2>/dev/null; then
    N8N_SIZE=$(du -h "$N8N_FILE" | cut -f1)
    echo "[OK]    n8n → $N8N_FILE ($N8N_SIZE)"
else
    echo "[ERROR] n8n dump failed!"
    ERRORS=$((ERRORS + 1))
fi

# ---------------------------------------------------------------------------
# 3. Integrity check — pg_restore --list on both dumps
# ---------------------------------------------------------------------------
echo ""
echo "[STEP]  Running integrity checks (pg_restore --list)..."

for dump_file in "$CTX_FILE" "$N8N_FILE"; do
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
# 4. Rotation — delete backups older than RETENTION_DAYS
# ---------------------------------------------------------------------------
echo ""
echo "[STEP]  Rotating backups older than ${RETENTION_DAYS} days..."

DELETED=0
while IFS= read -r -d '' old_file; do
    echo "[DEL]   $(basename "$old_file")"
    rm -f "$old_file"
    DELETED=$((DELETED + 1))
done < <(find "$BACKUP_DIR" -maxdepth 1 -name "*.dump" -mtime +${RETENTION_DAYS} -print0 2>/dev/null)

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
