#!/bin/bash
# =============================================================================
# init-data.sh — Idempotent Context Store DB Initialization
# =============================================================================
# Reproduces the full DB state: base schema + Phase 0 + Phase 1 + Guard.
#
# Design principles:
#   - Every statement uses IF NOT EXISTS / DO-block guards -> fully idempotent
#   - Extensions created as superuser, schema objects as context_user
#   - GENERATED COLUMNS require their extension FIRST (pgcrypto for digest())
#   - CREATE INDEX CONCURRENTLY cannot run inside a transaction block
#     -> separated into standalone psql -c calls
#   - HEREDOC quoting: <<-EOSQL (unquoted) for sections needing shell vars,
#     <<-'EOSQL' (quoted) for pure SQL with $$ delimiters (no escaping needed)
#   - Order: Extensions -> Tables -> Columns -> Constraints -> Indexes -> Functions -> Triggers
#
# Execution context:
#   Called by PostgreSQL entrypoint (/docker-entrypoint-initdb.d/) on first start,
#   or manually via: docker exec n8n-db-1 bash /docker-entrypoint-initdb.d/init-data.sh
#
# Idempotency:
#   Running this on an existing DB must produce zero errors — only NOTICEs.
# =============================================================================
# Part of ctx by GottZ — The memory your LLM pretends to have.
# Implements the GottZ Scope Model (multi-tenant isolation) and
# GottZ Guard (async duplicate detection) at the schema level.
# Source: https://github.com/GottZ/ctx
set -e

# =============================================================================
# SECTION 1: n8n Application User + DB Grants
# =============================================================================
if [ -n "${POSTGRES_NON_ROOT_USER:-}" ] && [ -n "${POSTGRES_NON_ROOT_PASSWORD:-}" ]; then
    echo "SETUP [1/7]: Creating n8n application user..."
    psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
		DO \$\$
		BEGIN
		    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '${POSTGRES_NON_ROOT_USER}') THEN
		        CREATE USER ${POSTGRES_NON_ROOT_USER} WITH PASSWORD '${POSTGRES_NON_ROOT_PASSWORD}';
		    END IF;
		END
		\$\$;
		GRANT ALL PRIVILEGES ON DATABASE ${POSTGRES_DB} TO ${POSTGRES_NON_ROOT_USER};
		GRANT CREATE ON SCHEMA public TO ${POSTGRES_NON_ROOT_USER};
	EOSQL
    echo "SETUP [1/7]: n8n user ready."
else
    echo "SETUP [1/7]: SKIP — no n8n user env vars."
fi

# =============================================================================
# Exit early if context store env vars are not set
# =============================================================================
if [ -z "${CONTEXT_DB:-}" ] || [ -z "${CONTEXT_DB_USER:-}" ] || [ -z "${CONTEXT_DB_PASSWORD:-}" ]; then
    echo "SETUP: No context store environment variables given, skipping sections 2-7."
    exit 0
fi

# =============================================================================
# SECTION 2: Context Store Database + User
# =============================================================================
echo "SETUP [2/7]: Creating context_store database and user..."

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

echo "SETUP [2/7]: Database and user ready."

# =============================================================================
# SECTION 3: Extensions (as superuser, on context_store DB)
# =============================================================================
echo "SETUP [3/7]: Creating extensions..."

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "${CONTEXT_DB}" <<-EOSQL
	GRANT CREATE ON SCHEMA public TO ${CONTEXT_DB_USER};

	CREATE EXTENSION IF NOT EXISTS vector;
	CREATE EXTENSION IF NOT EXISTS timescaledb;
	CREATE EXTENSION IF NOT EXISTS pgcrypto;
	CREATE EXTENSION IF NOT EXISTS pg_trgm;
EOSQL

echo "SETUP [3/7]: Extensions ready (vector, timescaledb, pgcrypto, pg_trgm)."

# =============================================================================
# SECTION 4: Base Tables (as context_user)
# =============================================================================
echo "SETUP [4/7]: Creating base tables..."

PGPASSWORD="${CONTEXT_DB_PASSWORD}" psql -v ON_ERROR_STOP=1 --username "${CONTEXT_DB_USER}" --dbname "${CONTEXT_DB}" <<-'EOSQL'

	-- -----------------------------------------------------------------
	-- context_blocks — main knowledge store
	-- -----------------------------------------------------------------
	CREATE TABLE IF NOT EXISTS context_blocks (
	    id              UUID PRIMARY KEY DEFAULT uuidv7(),
	    category        VARCHAR(100)    NOT NULL,
	    tags            TEXT[]          DEFAULT '{}',
	    title           VARCHAR(255)    NOT NULL,
	    content         TEXT            NOT NULL,
	    metadata        JSONB           DEFAULT '{}',
	    embedding       vector(1024),
	    created_at      TIMESTAMPTZ     DEFAULT now(),
	    updated_at      TIMESTAMPTZ     DEFAULT now()
	);

	-- -----------------------------------------------------------------
	-- context_api_keys — multi-tenant key->scope mapping
	-- -----------------------------------------------------------------
	CREATE TABLE IF NOT EXISTS context_api_keys (
	    id              UUID PRIMARY KEY DEFAULT uuidv7(),
	    api_key         VARCHAR(128) NOT NULL UNIQUE,
	    key_hash        TEXT,
	    label           VARCHAR(100) NOT NULL,
	    home_scope      VARCHAR(20)  NOT NULL,
	    allowed_scopes  TEXT[]       NOT NULL DEFAULT '{shared}',
	    active          BOOLEAN      NOT NULL DEFAULT true,
	    last_used_at    TIMESTAMPTZ,
	    created_at      TIMESTAMPTZ  DEFAULT now(),
	    updated_at      TIMESTAMPTZ  DEFAULT now()
	);

	-- -----------------------------------------------------------------
	-- context_digest_state — singleton row tracking digest freshness
	-- -----------------------------------------------------------------
	CREATE TABLE IF NOT EXISTS context_digest_state (
	    id              BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
	    dirty_since     TIMESTAMPTZ,
	    last_digest_at  TIMESTAMPTZ
	);
	INSERT INTO context_digest_state (id) VALUES (true) ON CONFLICT DO NOTHING;

	-- -----------------------------------------------------------------
	-- context_guard_state — singleton row tracking guard freshness
	-- -----------------------------------------------------------------
	CREATE TABLE IF NOT EXISTS context_guard_state (
	    id              BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
	    dirty_since     TIMESTAMPTZ,
	    last_guard_at   TIMESTAMPTZ,
	    pending_count   INTEGER DEFAULT 0
	);
	INSERT INTO context_guard_state (id) VALUES (true) ON CONFLICT DO NOTHING;

	-- -----------------------------------------------------------------
	-- context_blobs — binary asset storage
	-- -----------------------------------------------------------------
	CREATE TABLE IF NOT EXISTS context_blobs (
	    id              UUID PRIMARY KEY DEFAULT uuidv7(),
	    context_block_id UUID REFERENCES context_blocks(id) ON DELETE SET NULL,
	    category        TEXT         NOT NULL,
	    title           TEXT         NOT NULL,
	    filename        TEXT         NOT NULL,
	    mime_type       TEXT         NOT NULL,
	    file_size       BIGINT       NOT NULL,
	    checksum        TEXT,
	    storage_type    TEXT         NOT NULL DEFAULT 'db',
	    data            BYTEA,
	    file_path       TEXT,
	    tags            TEXT[],
	    metadata        JSONB        DEFAULT '{}',
	    created_at      TIMESTAMPTZ  DEFAULT now(),
	    scope           VARCHAR(20)  NOT NULL DEFAULT 'private',
	    updated_at      TIMESTAMPTZ  DEFAULT now(),
	    UNIQUE (category, title),
	    CHECK (
	        (storage_type = 'db'  AND data IS NOT NULL AND file_path IS NULL) OR
	        (storage_type = 'fs'  AND data IS NULL     AND file_path IS NOT NULL)
	    )
	);

	-- -----------------------------------------------------------------
	-- Phase 1: context_access_log — read audit trail
	-- -----------------------------------------------------------------
	CREATE TABLE IF NOT EXISTS context_access_log (
	    id              UUID PRIMARY KEY DEFAULT uuidv7(),
	    block_id        UUID REFERENCES context_blocks(id) ON DELETE SET NULL,
	    api_key_id      UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,
	    action          VARCHAR(50)  NOT NULL,
	    query_text      TEXT,
	    metadata        JSONB        DEFAULT '{}',
	    created_at      TIMESTAMPTZ  DEFAULT now()
	);

	-- -----------------------------------------------------------------
	-- Phase 1: context_write_log — write audit trail (v4 guard schema)
	-- -----------------------------------------------------------------
	CREATE TABLE IF NOT EXISTS context_write_log (
	    id               uuid          NOT NULL DEFAULT uuidv7(),
	    block_id         uuid          REFERENCES context_blocks(id) ON DELETE SET NULL,
	    matched_block_id uuid          REFERENCES context_blocks(id) ON DELETE SET NULL,
	    decision         varchar(20)   NOT NULL,
	    similarity       real,
	    scope            varchar(20),
	    block_title      varchar(500),
	    block_category   varchar(100),
	    api_key_id       uuid          REFERENCES context_api_keys(id) ON DELETE SET NULL,
	    metadata         jsonb         DEFAULT '{}'::jsonb,
	    created_at       timestamptz   DEFAULT now(),
	    CONSTRAINT context_write_log_pkey PRIMARY KEY (id)
	);

EOSQL

echo "SETUP [4/7]: Base tables ready."

# =============================================================================
# SECTION 5: Column Additions + Constraints (idempotent ALTER TABLE)
# =============================================================================
echo "SETUP [5/7]: Adding columns and constraints..."

PGPASSWORD="${CONTEXT_DB_PASSWORD}" psql -v ON_ERROR_STOP=1 --username "${CONTEXT_DB_USER}" --dbname "${CONTEXT_DB}" <<-'EOSQL'

	-- -- context_blocks: scope column --
	ALTER TABLE context_blocks
	    ADD COLUMN IF NOT EXISTS scope VARCHAR(20) NOT NULL DEFAULT 'private';

	-- -- context_blocks: Phase 0 columns --
	-- content_hash: GENERATED ALWAYS — requires pgcrypto (created in Section 3)
	DO $$
	BEGIN
	    IF NOT EXISTS (
	        SELECT 1 FROM information_schema.columns
	        WHERE table_name = 'context_blocks' AND column_name = 'content_hash'
	    ) THEN
	        ALTER TABLE context_blocks
	            ADD COLUMN content_hash VARCHAR(64)
	            GENERATED ALWAYS AS (encode(digest(content, 'sha256'), 'hex')) STORED;
	    END IF;
	END
	$$;

	ALTER TABLE context_blocks
	    ADD COLUMN IF NOT EXISTS is_archived   BOOLEAN NOT NULL DEFAULT false;
	ALTER TABLE context_blocks
	    ADD COLUMN IF NOT EXISTS superseded_by UUID;
	ALTER TABLE context_blocks
	    ADD COLUMN IF NOT EXISTS temporal      BOOLEAN NOT NULL DEFAULT false;
	ALTER TABLE context_blocks
	    ADD COLUMN IF NOT EXISTS embed_model   TEXT DEFAULT 'qwen3-embedding:8b';

	-- -- context_blocks: guard_status column --
	ALTER TABLE context_blocks
	    ADD COLUMN IF NOT EXISTS guard_status  VARCHAR(20) DEFAULT 'active';

	-- -- context_blocks: Phase 1 tsvector GENERATED columns --
	DO $$
	BEGIN
	    IF NOT EXISTS (
	        SELECT 1 FROM information_schema.columns
	        WHERE table_name = 'context_blocks' AND column_name = 'ts_de'
	    ) THEN
	        ALTER TABLE context_blocks
	            ADD COLUMN ts_de TSVECTOR
	            GENERATED ALWAYS AS (
	                to_tsvector('german', coalesce(title::text, '') || ' ' || content)
	            ) STORED;
	    END IF;
	END
	$$;

	DO $$
	BEGIN
	    IF NOT EXISTS (
	        SELECT 1 FROM information_schema.columns
	        WHERE table_name = 'context_blocks' AND column_name = 'ts_en'
	    ) THEN
	        ALTER TABLE context_blocks
	            ADD COLUMN ts_en TSVECTOR
	            GENERATED ALWAYS AS (
	                to_tsvector('english', coalesce(title::text, '') || ' ' || content)
	            ) STORED;
	    END IF;
	END
	$$;

	-- -- Unique constraint on context_blocks (category, title) --
	DO $$
	BEGIN
	    IF NOT EXISTS (
	        SELECT 1 FROM pg_constraint
	        WHERE conrelid = 'context_blocks'::regclass
	          AND conname  = 'uq_context_category_title'
	    ) THEN
	        ALTER TABLE context_blocks
	            ADD CONSTRAINT uq_context_category_title UNIQUE (category, title);
	    END IF;
	END
	$$;

	-- -- CHECK constraint on scope values --
	DO $$
	BEGIN
	    IF NOT EXISTS (
	        SELECT 1 FROM pg_constraint
	        WHERE conrelid = 'context_blocks'::regclass
	          AND conname  = 'chk_scope'
	    ) THEN
	        ALTER TABLE context_blocks
	            ADD CONSTRAINT chk_scope CHECK (scope IN ('private', 'work', 'shared'));
	    END IF;
	END
	$$;

	-- -- context_api_keys: key_hash + last_used_at columns (idempotent) --
	ALTER TABLE context_api_keys ADD COLUMN IF NOT EXISTS key_hash TEXT;
	ALTER TABLE context_api_keys ADD COLUMN IF NOT EXISTS last_used_at TIMESTAMPTZ;

	-- Backfill key_hash for existing rows
	UPDATE context_api_keys
	    SET key_hash = encode(digest(api_key, 'sha256'), 'hex')
	    WHERE key_hash IS NULL;

	-- -- context_blobs: scope column --
	ALTER TABLE context_blobs
	    ADD COLUMN IF NOT EXISTS scope VARCHAR(20) NOT NULL DEFAULT 'private';

	-- -- context_blobs: CHECK constraint on scope values --
	DO $$
	BEGIN
	    IF NOT EXISTS (
	        SELECT 1 FROM pg_constraint
	        WHERE conrelid = 'context_blobs'::regclass
	          AND conname  = 'chk_blob_scope'
	    ) THEN
	        ALTER TABLE context_blobs
	            ADD CONSTRAINT chk_blob_scope CHECK (scope IN ('private', 'work', 'shared'));
	    END IF;
	END
	$$;

EOSQL

echo "SETUP [5/7]: Columns and constraints ready."

# =============================================================================
# SECTION 6: Indexes (regular — inside transaction is fine)
# =============================================================================
echo "SETUP [6/7]: Creating indexes..."

PGPASSWORD="${CONTEXT_DB_PASSWORD}" psql -v ON_ERROR_STOP=1 --username "${CONTEXT_DB_USER}" --dbname "${CONTEXT_DB}" <<-'EOSQL'

	-- -- context_blocks: base indexes --
	CREATE INDEX IF NOT EXISTS idx_context_category
	    ON context_blocks(category);
	CREATE INDEX IF NOT EXISTS idx_context_tags
	    ON context_blocks USING GIN(tags);
	CREATE INDEX IF NOT EXISTS idx_context_metadata
	    ON context_blocks USING GIN(metadata);
	CREATE INDEX IF NOT EXISTS idx_context_created
	    ON context_blocks(created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_context_scope
	    ON context_blocks(scope);

	-- -- context_blocks: Phase 0 indexes --
	CREATE INDEX IF NOT EXISTS idx_content_hash
	    ON context_blocks(content_hash);
	CREATE INDEX IF NOT EXISTS idx_archived
	    ON context_blocks(is_archived) WHERE is_archived = true;

	-- -- context_blocks: Phase 1 fulltext indexes (on GENERATED tsvector columns) --
	CREATE INDEX IF NOT EXISTS idx_context_ts_de
	    ON context_blocks USING GIN(ts_de);
	CREATE INDEX IF NOT EXISTS idx_context_ts_en
	    ON context_blocks USING GIN(ts_en);

	-- -- context_blocks: Phase 1 trigram indexes --
	CREATE INDEX IF NOT EXISTS idx_trgm_content
	    ON context_blocks USING GIN(content gin_trgm_ops);
	CREATE INDEX IF NOT EXISTS idx_trgm_title
	    ON context_blocks USING GIN(title gin_trgm_ops);

	-- -- context_blocks: guard_status index --
	CREATE INDEX IF NOT EXISTS idx_guard_status
	    ON context_blocks(guard_status) WHERE guard_status != 'active';

	-- -- context_api_keys indexes --
	CREATE INDEX IF NOT EXISTS idx_api_keys_key
	    ON context_api_keys(api_key) WHERE active = true;
	CREATE INDEX IF NOT EXISTS idx_api_keys_hash
	    ON context_api_keys(key_hash);

	-- -- context_blobs indexes --
	CREATE INDEX IF NOT EXISTS idx_blobs_block_ref
	    ON context_blobs(context_block_id);
	CREATE INDEX IF NOT EXISTS idx_blobs_category_created
	    ON context_blobs(category, created_at);
	CREATE INDEX IF NOT EXISTS idx_blobs_metadata
	    ON context_blobs USING GIN(metadata);
	CREATE INDEX IF NOT EXISTS idx_blobs_mime
	    ON context_blobs(mime_type);
	CREATE INDEX IF NOT EXISTS idx_blobs_storage
	    ON context_blobs(storage_type);
	CREATE INDEX IF NOT EXISTS idx_blobs_tags
	    ON context_blobs USING GIN(tags);
	CREATE INDEX IF NOT EXISTS idx_blobs_scope
	    ON context_blobs(scope);

	-- -- context_access_log indexes (Phase 1) --
	CREATE INDEX IF NOT EXISTS idx_access_log_block
	    ON context_access_log(block_id);
	CREATE INDEX IF NOT EXISTS idx_access_log_created
	    ON context_access_log(created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_access_log_api_key
	    ON context_access_log(api_key_id);
	CREATE INDEX IF NOT EXISTS idx_access_log_action
	    ON context_access_log(action);

	-- -- context_write_log indexes (Phase 1, v4 guard schema) --
	CREATE INDEX IF NOT EXISTS idx_write_log_created
	    ON context_write_log(created_at DESC);
	CREATE INDEX IF NOT EXISTS idx_write_log_block
	    ON context_write_log(block_id);
	CREATE INDEX IF NOT EXISTS idx_write_log_decision
	    ON context_write_log(decision);
	CREATE INDEX IF NOT EXISTS idx_write_log_scope
	    ON context_write_log(scope);

EOSQL

echo "SETUP [6/7]: Regular indexes ready."

# =============================================================================
# SECTION 6b: HNSW Index (CONCURRENTLY — must be outside transaction block)
# =============================================================================
echo "SETUP [6b/7]: Creating HNSW index (CONCURRENTLY, outside transaction)..."

PGPASSWORD="${CONTEXT_DB_PASSWORD}" psql --username "${CONTEXT_DB_USER}" --dbname "${CONTEXT_DB}" \
    -c "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_embedding_hnsw ON context_blocks USING hnsw ((embedding::halfvec(1024)) halfvec_cosine_ops) WITH (m = 16, ef_construction = 64);" \
    || echo "WARNING: HNSW index creation returned non-zero (may already exist, continuing)."

echo "SETUP [6b/7]: HNSW index done."

# =============================================================================
# SECTION 6c: Storage type overrides (ALTER COLUMN SET STORAGE)
# =============================================================================
echo "SETUP [6c/7]: Setting column storage types..."

PGPASSWORD="${CONTEXT_DB_PASSWORD}" psql -v ON_ERROR_STOP=1 --username "${CONTEXT_DB_USER}" --dbname "${CONTEXT_DB}" <<-'EOSQL'
	-- embedding: EXTERNAL (skip TOAST compression, vectors are incompressible)
	ALTER TABLE context_blocks ALTER COLUMN embedding SET STORAGE EXTERNAL;
	-- blobs data: EXTERNAL (skip TOAST compression, binary data pre-compressed)
	ALTER TABLE context_blobs ALTER COLUMN data SET STORAGE EXTERNAL;
EOSQL

echo "SETUP [6c/7]: Storage types set."

# =============================================================================
# SECTION 7: Functions + Triggers
# =============================================================================
echo "SETUP [7/7]: Creating functions and triggers..."

PGPASSWORD="${CONTEXT_DB_PASSWORD}" psql -v ON_ERROR_STOP=1 --username "${CONTEXT_DB_USER}" --dbname "${CONTEXT_DB}" <<-'EOSQL'

	-- -- mark_digest_dirty() — auto-digest trigger function --
	CREATE OR REPLACE FUNCTION mark_digest_dirty()
	RETURNS trigger
	LANGUAGE plpgsql
	AS $$
	BEGIN
	    -- Skip index-category blocks (the digest itself) to avoid loops
	    IF TG_OP = 'DELETE' THEN
	        IF OLD.category = 'index' THEN RETURN OLD; END IF;
	    ELSE
	        IF NEW.category = 'index' THEN RETURN NEW; END IF;
	    END IF;

	    UPDATE context_digest_state SET dirty_since = COALESCE(dirty_since, now());

	    IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
	END;
	$$;

	-- -- Trigger: trg_digest_dirty on context_blocks --
	DROP TRIGGER IF EXISTS trg_digest_dirty ON context_blocks;
	CREATE TRIGGER trg_digest_dirty
	    AFTER INSERT OR UPDATE OR DELETE ON context_blocks
	    FOR EACH ROW
	    EXECUTE FUNCTION mark_digest_dirty();

	-- -- mark_guard_dirty() — guard trigger function --
	CREATE OR REPLACE FUNCTION mark_guard_dirty()
	RETURNS trigger
	LANGUAGE plpgsql
	AS $$
	BEGIN
	    -- Skip metadata-only guard updates (avoid infinite loop)
	    IF TG_OP = 'UPDATE' AND OLD.metadata ? 'guard_status' AND NEW.metadata ? 'guard_status' AND OLD.content = NEW.content THEN
	        RETURN NEW;
	    END IF;
	    -- Skip index category blocks
	    IF NEW.category = 'index' THEN RETURN NEW; END IF;
	    -- Mark guard state as dirty
	    UPDATE context_guard_state SET dirty_since = COALESCE(dirty_since, now()), pending_count = pending_count + 1;
	    RETURN NEW;
	END;
	$$;

	-- -- Trigger: trg_guard_dirty on context_blocks --
	DROP TRIGGER IF EXISTS trg_guard_dirty ON context_blocks;
	CREATE TRIGGER trg_guard_dirty
	    AFTER INSERT OR UPDATE ON context_blocks
	    FOR EACH ROW
	    EXECUTE FUNCTION mark_guard_dirty();

EOSQL

echo "SETUP [7/7]: Functions and triggers ready."

# =============================================================================
# DONE
# =============================================================================
echo "============================================================"
echo "SETUP COMPLETE: Context Store fully initialized."
echo "  Tables:     context_blocks, context_api_keys, context_blobs,"
echo "              context_digest_state, context_guard_state,"
echo "              context_access_log, context_write_log"
echo "  Extensions: vector, timescaledb, pgcrypto, pg_trgm"
echo "  Indexes:    16 regular + 1 HNSW (CONCURRENTLY)"
echo "  Triggers:   trg_digest_dirty, trg_guard_dirty"
echo "============================================================"
