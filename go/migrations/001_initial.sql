-- 001_initial.sql — Full Context Store schema
-- Extensions, tables, columns, constraints, indexes, triggers
-- All statements are idempotent (IF NOT EXISTS / CREATE OR REPLACE)

-- =============================================================================
-- Extensions (require superuser — run against context_store DB)
-- =============================================================================
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS vector;

-- =============================================================================
-- Tables
-- =============================================================================

-- context_blocks — main knowledge store
CREATE TABLE IF NOT EXISTS context_blocks (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    category        VARCHAR(100)    NOT NULL,
    tags            TEXT[]          DEFAULT '{}',
    title           VARCHAR(255)    NOT NULL,
    content         TEXT            NOT NULL,
    metadata        JSONB           DEFAULT '{}',
    embedding       vector(1024),
    created_at      TIMESTAMPTZ     DEFAULT now(),
    updated_at      TIMESTAMPTZ     DEFAULT now(),
    -- scope
    scope           VARCHAR(20)     NOT NULL DEFAULT 'private',
    -- Phase 0: content dedup + archival
    content_hash    VARCHAR(64)     GENERATED ALWAYS AS (encode(digest(content, 'sha256'), 'hex')) STORED,
    is_archived     BOOLEAN         NOT NULL DEFAULT false,
    superseded_by   UUID,
    temporal        BOOLEAN         NOT NULL DEFAULT false,
    embed_model     TEXT            DEFAULT 'qwen3-embedding:8b',
    -- guard
    guard_status    VARCHAR(20)     DEFAULT 'active',
    -- scale columns (Session 5)
    source_id       UUID,
    parent_id       UUID,
    block_type      VARCHAR(30)     DEFAULT 'knowledge',
    chunk_index     INTEGER,
    quality_score   REAL            DEFAULT 1.0,
    embed_status    VARCHAR(20)     DEFAULT 'done',
    description     TEXT,
    auto_tags       TEXT[]          DEFAULT '{}',
    language        VARCHAR(10),
    -- Phase 1: precomputed fulltext tsvectors
    ts_de           TSVECTOR        GENERATED ALWAYS AS (
                        to_tsvector('german', coalesce(title::text, '') || ' ' || content)
                    ) STORED,
    ts_en           TSVECTOR        GENERATED ALWAYS AS (
                        to_tsvector('english', coalesce(title::text, '') || ' ' || content)
                    ) STORED
);

-- context_api_keys — multi-tenant key->scope mapping
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

-- context_digest_state — singleton row tracking digest freshness
CREATE TABLE IF NOT EXISTS context_digest_state (
    id              BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
    dirty_since     TIMESTAMPTZ,
    last_digest_at  TIMESTAMPTZ
);
INSERT INTO context_digest_state (id) VALUES (true) ON CONFLICT DO NOTHING;

-- context_guard_state — singleton row tracking guard freshness
CREATE TABLE IF NOT EXISTS context_guard_state (
    id              BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
    dirty_since     TIMESTAMPTZ,
    last_guard_at   TIMESTAMPTZ,
    pending_count   INTEGER DEFAULT 0
);
INSERT INTO context_guard_state (id) VALUES (true) ON CONFLICT DO NOTHING;

-- context_blobs — binary asset storage
CREATE TABLE IF NOT EXISTS context_blobs (
    id               UUID PRIMARY KEY DEFAULT uuidv7(),
    context_block_id UUID REFERENCES context_blocks(id) ON DELETE SET NULL,
    category         TEXT         NOT NULL,
    title            TEXT         NOT NULL,
    filename         TEXT         NOT NULL,
    mime_type        TEXT         NOT NULL,
    file_size        BIGINT       NOT NULL,
    checksum         TEXT,
    storage_type     TEXT         NOT NULL DEFAULT 'db',
    data             BYTEA,
    file_path        TEXT,
    tags             TEXT[],
    metadata         JSONB        DEFAULT '{}',
    created_at       TIMESTAMPTZ  DEFAULT now(),
    scope            VARCHAR(20)  NOT NULL DEFAULT 'private',
    updated_at       TIMESTAMPTZ  DEFAULT now(),
    UNIQUE (category, title),
    CHECK (
        (storage_type = 'db'  AND data IS NOT NULL AND file_path IS NULL) OR
        (storage_type = 'fs'  AND data IS NULL     AND file_path IS NOT NULL)
    )
);

-- context_access_log — read audit trail
CREATE TABLE IF NOT EXISTS context_access_log (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    block_id        UUID REFERENCES context_blocks(id) ON DELETE SET NULL,
    api_key_id      UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,
    action          VARCHAR(50)  NOT NULL,
    query_text      TEXT,
    metadata        JSONB        DEFAULT '{}',
    created_at      TIMESTAMPTZ  DEFAULT now()
);

-- context_write_log — write audit trail (v4 guard schema)
CREATE TABLE IF NOT EXISTS context_write_log (
    id               UUID          NOT NULL DEFAULT uuidv7(),
    block_id         UUID          REFERENCES context_blocks(id) ON DELETE SET NULL,
    matched_block_id UUID          REFERENCES context_blocks(id) ON DELETE SET NULL,
    decision         VARCHAR(20)   NOT NULL,
    similarity       REAL,
    scope            VARCHAR(20),
    block_title      VARCHAR(500),
    block_category   VARCHAR(100),
    api_key_id       UUID          REFERENCES context_api_keys(id) ON DELETE SET NULL,
    metadata         JSONB         DEFAULT '{}'::jsonb,
    created_at       TIMESTAMPTZ   DEFAULT now(),
    CONSTRAINT context_write_log_pkey PRIMARY KEY (id)
);

-- =============================================================================
-- Constraints (idempotent via DO blocks)
-- =============================================================================

-- Unique constraint on context_blocks (category, title)
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

-- CHECK constraint on context_blocks scope values
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

-- CHECK constraint on context_blobs scope values
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

-- Backfill key_hash for existing rows
UPDATE context_api_keys
    SET key_hash = encode(digest(api_key, 'sha256'), 'hex')
    WHERE key_hash IS NULL;

-- =============================================================================
-- Indexes — context_blocks
-- =============================================================================
CREATE INDEX IF NOT EXISTS idx_context_category     ON context_blocks(category);
CREATE INDEX IF NOT EXISTS idx_context_tags          ON context_blocks USING GIN(tags);
CREATE INDEX IF NOT EXISTS idx_context_metadata      ON context_blocks USING GIN(metadata);
CREATE INDEX IF NOT EXISTS idx_context_created       ON context_blocks(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_context_scope         ON context_blocks(scope);
CREATE INDEX IF NOT EXISTS idx_content_hash          ON context_blocks(content_hash);
CREATE INDEX IF NOT EXISTS idx_archived              ON context_blocks(is_archived) WHERE is_archived = true;
CREATE INDEX IF NOT EXISTS idx_context_ts_de         ON context_blocks USING GIN(ts_de);
CREATE INDEX IF NOT EXISTS idx_context_ts_en         ON context_blocks USING GIN(ts_en);
CREATE INDEX IF NOT EXISTS idx_trgm_title            ON context_blocks USING GIN(title gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_source_id             ON context_blocks(source_id);
CREATE INDEX IF NOT EXISTS idx_parent_id             ON context_blocks(parent_id);
CREATE INDEX IF NOT EXISTS idx_block_type            ON context_blocks(block_type);
CREATE INDEX IF NOT EXISTS idx_embed_pending         ON context_blocks(embed_status) WHERE embed_status != 'done';
CREATE INDEX IF NOT EXISTS idx_auto_tags             ON context_blocks USING GIN(auto_tags);
CREATE INDEX IF NOT EXISTS idx_language              ON context_blocks(language);
CREATE INDEX IF NOT EXISTS idx_guard_status          ON context_blocks(guard_status) WHERE guard_status != 'active';

-- =============================================================================
-- Indexes — context_api_keys
-- =============================================================================
CREATE INDEX IF NOT EXISTS idx_api_keys_key          ON context_api_keys(api_key) WHERE active = true;
CREATE INDEX IF NOT EXISTS idx_api_keys_hash         ON context_api_keys(key_hash);

-- =============================================================================
-- Indexes — context_blobs
-- =============================================================================
CREATE INDEX IF NOT EXISTS idx_blobs_block_ref       ON context_blobs(context_block_id);
CREATE INDEX IF NOT EXISTS idx_blobs_category_created ON context_blobs(category, created_at);
CREATE INDEX IF NOT EXISTS idx_blobs_metadata        ON context_blobs USING GIN(metadata);
CREATE INDEX IF NOT EXISTS idx_blobs_mime            ON context_blobs(mime_type);
CREATE INDEX IF NOT EXISTS idx_blobs_storage         ON context_blobs(storage_type);
CREATE INDEX IF NOT EXISTS idx_blobs_tags            ON context_blobs USING GIN(tags);
CREATE INDEX IF NOT EXISTS idx_blobs_scope           ON context_blobs(scope);

-- =============================================================================
-- Indexes — context_access_log
-- =============================================================================
CREATE INDEX IF NOT EXISTS idx_access_log_block      ON context_access_log(block_id);
CREATE INDEX IF NOT EXISTS idx_access_log_created    ON context_access_log(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_access_log_api_key    ON context_access_log(api_key_id);
CREATE INDEX IF NOT EXISTS idx_access_log_action     ON context_access_log(action);

-- =============================================================================
-- Indexes — context_write_log
-- =============================================================================
CREATE INDEX IF NOT EXISTS idx_write_log_created     ON context_write_log(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_write_log_block       ON context_write_log(block_id);
CREATE INDEX IF NOT EXISTS idx_write_log_decision    ON context_write_log(decision);
CREATE INDEX IF NOT EXISTS idx_write_log_scope       ON context_write_log(scope);

-- =============================================================================
-- HNSW Index (halfvec cosine, m=16, ef_construction=64)
-- NOTE: In init-data.sh this ran CONCURRENTLY outside a transaction.
-- Within a migration transaction we drop CONCURRENTLY — the index is small
-- enough at current scale. For large DBs, run manually outside a transaction.
-- =============================================================================
CREATE INDEX IF NOT EXISTS idx_embedding_hnsw
    ON context_blocks USING hnsw ((embedding::halfvec(1024)) halfvec_cosine_ops)
    WITH (m = 16, ef_construction = 64);

-- =============================================================================
-- Storage type overrides
-- =============================================================================
ALTER TABLE context_blocks ALTER COLUMN embedding SET STORAGE EXTERNAL;
ALTER TABLE context_blobs  ALTER COLUMN data      SET STORAGE EXTERNAL;

-- =============================================================================
-- Functions + Triggers
-- =============================================================================

-- mark_digest_dirty() — auto-digest trigger function
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

DROP TRIGGER IF EXISTS trg_digest_dirty ON context_blocks;
CREATE TRIGGER trg_digest_dirty
    AFTER INSERT OR UPDATE OR DELETE ON context_blocks
    FOR EACH ROW
    EXECUTE FUNCTION mark_digest_dirty();

-- mark_guard_dirty() — guard trigger function
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

DROP TRIGGER IF EXISTS trg_guard_dirty ON context_blocks;
CREATE TRIGGER trg_guard_dirty
    AFTER INSERT OR UPDATE ON context_blocks
    FOR EACH ROW
    EXECUTE FUNCTION mark_guard_dirty();
