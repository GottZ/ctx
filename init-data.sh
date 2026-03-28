#!/bin/bash
# =============================================================================
# init-data.sh — Context Store Bootstrap (DB, User, Extensions)
# =============================================================================
# Creates the context_store database, user, and extensions as superuser.
# Schema (tables, indexes, triggers) is managed by Go embedded migrations.
#
# Execution context:
#   Called by PostgreSQL entrypoint (/docker-entrypoint-initdb.d/) on first start,
#   or manually via: docker exec n8n-db-1 bash /docker-entrypoint-initdb.d/init-data.sh
#
# Idempotency:
#   Running this on an existing DB must produce zero errors — only NOTICEs.
# =============================================================================
# Part of ctx by GottZ — The memory your LLM pretends to have.
# Source: https://github.com/GottZ/ctx
set -e

# =============================================================================
# Exit early if context store env vars are not set
# =============================================================================
if [ -z "${CONTEXT_DB:-}" ] || [ -z "${CONTEXT_DB_USER:-}" ] || [ -z "${CONTEXT_DB_PASSWORD:-}" ]; then
    echo "SETUP: No context store environment variables given, skipping."
    exit 0
fi

# =============================================================================
# SECTION 1: Context Store Database + User (requires superuser)
# =============================================================================
echo "SETUP [1/2]: Creating context_store database and user..."

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
	SELECT 'CREATE DATABASE ${CONTEXT_DB}'
	WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '${CONTEXT_DB}')
	\gexec

	DO \$\$
	BEGIN
	    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '${CONTEXT_DB_USER}') THEN
	        CREATE USER ${CONTEXT_DB_USER} WITH PASSWORD '${CONTEXT_DB_PASSWORD}';
	    END IF;
	END
	\$\$;
	GRANT ALL PRIVILEGES ON DATABASE ${CONTEXT_DB} TO ${CONTEXT_DB_USER};
EOSQL

echo "SETUP [1/2]: Database and user ready."

# =============================================================================
# SECTION 2: Extensions (as superuser, on context_store DB)
# =============================================================================
echo "SETUP [2/2]: Creating extensions..."

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "${CONTEXT_DB}" <<-EOSQL
	GRANT CREATE ON SCHEMA public TO ${CONTEXT_DB_USER};

	CREATE EXTENSION IF NOT EXISTS vector;
	CREATE EXTENSION IF NOT EXISTS timescaledb;
	CREATE EXTENSION IF NOT EXISTS pgcrypto;
	CREATE EXTENSION IF NOT EXISTS pg_trgm;
EOSQL

echo "SETUP [2/2]: Extensions ready (vector, timescaledb, pgcrypto, pg_trgm)."

# =============================================================================
# DONE — Schema is handled by ctx Go server (embedded migrations)
# =============================================================================
echo "SETUP COMPLETE: Context Store bootstrap done."
echo "  Database:   ${CONTEXT_DB}"
echo "  User:       ${CONTEXT_DB_USER}"
echo "  Extensions: vector, timescaledb, pgcrypto, pg_trgm"
