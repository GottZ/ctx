-- =============================================================================
-- 113_baseline.sql — consolidated baseline for migrations 001-113
-- =============================================================================
-- v5 requires the last v4.x release (v4.38.0) as its upgrade floor
-- (docs/operations.md, "The v4.x hop is mandatory"), so every database that
-- reaches v5 through the supported path already carries versions 001-132.
-- That makes the individual files below dead weight on the upgrade path and
-- pure setup cost on the fresh-install path — this file replaces them.
--
-- Fold line = 113, derived, not chosen: 114 is the lowest version any test
-- reproduces as an intermediate database state
-- (internal/schemacontract/migration115_hnsw_reconcile_integration_test.go
-- caps the chain at 114), so 113 is the highest version that can be folded
-- without destroying a reachable point in the chain.
--
-- HOW THE TWO PATHS ARE SERVED — both by the runner's existing per-version
-- bookkeeping in _migrations, no new mechanism:
--   fresh install: version 113 is absent -> this file runs, then 114-133.
--   upgrade from v4.38.0: version 113 is present (as 113_embed_failures.sql)
--     -> this file is skipped, only 133 runs.
--   database below the fold with rows in _migrations: rejected loudly by
--     store.RunMigrations' baseline precondition — do the v4.38.0 hop first.
--
-- The folded files are embedded VERBATIM between "-- @@ ctx-fold" markers, so
-- migrations.Section(name) hands any caller the original bytes back and every
-- checksum this file writes into _migrations is the checksum of text that is
-- still here. The only edit is the removal of standalone BEGIN;/COMMIT; lines
-- from the two files that carried their own transaction block (043, 044) —
-- the runner supplies the transaction; see internal/store/migrations.go.
--
-- Regenerating this file is not possible after the fact: the source files are
-- gone from the tree. Git keeps them — `git show 5132b28e:go/migrations/` is
-- the last commit that carried the unfolded chain. This file was produced
-- mechanically from it, in version order.
--
-- The version ledger at the end of this file records every folded version
-- under its ORIGINAL filename and checksum, so a freshly installed database
-- and one upgraded through v4.38.0 carry the same _migrations content.
-- =============================================================================

-- @@ ctx-fold begin 001_initial.sql
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
-- @@ ctx-fold end 001_initial.sql

-- @@ ctx-fold begin 002_scale_columns.sql
-- 002_scale_columns.sql — Session 5 Scale-Preparation Columns
-- These columns are already included in 001_initial.sql's CREATE TABLE.
-- This migration exists for databases created before Session 5 that need
-- the columns added retroactively. All statements are idempotent.

-- Chunk hierarchy
ALTER TABLE context_blocks ADD COLUMN IF NOT EXISTS source_id      UUID;
ALTER TABLE context_blocks ADD COLUMN IF NOT EXISTS parent_id      UUID;
ALTER TABLE context_blocks ADD COLUMN IF NOT EXISTS block_type     VARCHAR(30) DEFAULT 'knowledge';
ALTER TABLE context_blocks ADD COLUMN IF NOT EXISTS chunk_index    INTEGER;

-- Quality + embedding queue
ALTER TABLE context_blocks ADD COLUMN IF NOT EXISTS quality_score  REAL DEFAULT 1.0;
ALTER TABLE context_blocks ADD COLUMN IF NOT EXISTS embed_status   VARCHAR(20) DEFAULT 'done';

-- Content enrichment
ALTER TABLE context_blocks ADD COLUMN IF NOT EXISTS description    TEXT;
ALTER TABLE context_blocks ADD COLUMN IF NOT EXISTS auto_tags      TEXT[] DEFAULT '{}';
ALTER TABLE context_blocks ADD COLUMN IF NOT EXISTS language       VARCHAR(10);

-- Indexes for scale columns (idempotent)
CREATE INDEX IF NOT EXISTS idx_source_id      ON context_blocks(source_id);
CREATE INDEX IF NOT EXISTS idx_parent_id      ON context_blocks(parent_id);
CREATE INDEX IF NOT EXISTS idx_block_type     ON context_blocks(block_type);
CREATE INDEX IF NOT EXISTS idx_embed_pending  ON context_blocks(embed_status) WHERE embed_status != 'done';
CREATE INDEX IF NOT EXISTS idx_auto_tags      ON context_blocks USING GIN(auto_tags);
CREATE INDEX IF NOT EXISTS idx_language       ON context_blocks(language);
-- @@ ctx-fold end 002_scale_columns.sql

-- @@ ctx-fold begin 003_pg_functions.sql
-- =============================================================================
-- 003_pg_functions.sql — PG Functions for ctx business logic
-- =============================================================================
-- Three functions that encapsulate core data logic:
--   1. ctx_auth(api_key)       — authenticate API key, return scopes
--   2. ctx_rrf(...)            — 4-way weighted Reciprocal Rank Fusion search
--   3. ctx_guard_check(block_id) — duplicate detection for a single block
--
-- Prerequisites: pgcrypto, pg_trgm, vector extensions
-- Target DB: context_store (user: context_user)
-- =============================================================================

-- -----------------------------------------------------------------------------
-- 1. ctx_auth — Authenticate API key and return scope information
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION ctx_auth(p_api_key TEXT)
RETURNS TABLE (
    api_key_id UUID,
    home_scope VARCHAR(20),
    allowed_scopes TEXT[],
    read_scopes TEXT[],
    is_valid BOOLEAN
) LANGUAGE plpgsql AS $$
DECLARE
    v_key_hash TEXT;
    v_api_key_id UUID;
    v_home_scope VARCHAR(20);
    v_allowed_scopes TEXT[];
BEGIN
    -- Compute SHA-256 hash of the provided API key
    v_key_hash := encode(digest(p_api_key, 'sha256'), 'hex');

    -- Authenticate: update last_used_at and return key info
    UPDATE context_api_keys
    SET last_used_at = now()
    WHERE context_api_keys.key_hash = v_key_hash
      AND context_api_keys.active = true
    RETURNING
        context_api_keys.id,
        context_api_keys.home_scope,
        context_api_keys.allowed_scopes
    INTO v_api_key_id, v_home_scope, v_allowed_scopes;

    -- Check if we found a valid key
    IF v_api_key_id IS NULL THEN
        -- Invalid key: return sentinel values
        api_key_id     := NULL;
        home_scope     := '__UNAUTHORIZED__';
        allowed_scopes := '{}'::TEXT[];
        read_scopes    := '{}'::TEXT[];
        is_valid       := false;
        RETURN NEXT;
        RETURN;
    END IF;

    -- Valid key: build read_scopes = [home_scope] || allowed_scopes
    api_key_id     := v_api_key_id;
    home_scope     := v_home_scope;
    allowed_scopes := v_allowed_scopes;
    read_scopes    := ARRAY[v_home_scope::TEXT] || COALESCE(v_allowed_scopes, '{}'::TEXT[]);
    is_valid       := true;
    RETURN NEXT;
    RETURN;
END;
$$;


-- -----------------------------------------------------------------------------
-- 2. ctx_rrf — 4-Way Weighted Reciprocal Rank Fusion
-- -----------------------------------------------------------------------------
-- Weights: Semantic 0.45, EN-FTS 0.25, DE-FTS 0.20, Trigram 0.10 (k=60)
-- Semantic LIMIT 75, Trigram LIMIT 30 (min similarity 0.05)
-- All CTEs scope-filtered, optional category + tags
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION ctx_rrf(
    p_embedding    halfvec(1024),
    p_query        TEXT,
    p_query_spaced TEXT,
    p_scopes       TEXT[],
    p_category     TEXT DEFAULT NULL,
    p_tags         TEXT[] DEFAULT NULL,
    p_limit        INT DEFAULT 5
) RETURNS TABLE (
    rrf_score   DOUBLE PRECISION,
    cosine_sim  DOUBLE PRECISION,
    id          UUID,
    title       VARCHAR(255),
    category    VARCHAR(100),
    tags        TEXT[],
    content     TEXT,
    scope       VARCHAR(20),
    updated_at  TIMESTAMPTZ
) LANGUAGE plpgsql AS $$
BEGIN
    -- Enable relaxed HNSW iterative scan for this transaction
    SET LOCAL hnsw.iterative_scan = 'relaxed_order';

    RETURN QUERY
    WITH semantic AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
            ) AS rank,
            1.0 - (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS cos_sim
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
        ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
        LIMIT 75
    ),
    fulltext_de AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query)),
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced))
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_de @@ plainto_tsquery('german', p_query)
              OR cb.ts_de @@ plainto_tsquery('german', p_query_spaced)
          )
        LIMIT 100
    ),
    fulltext_en AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced))
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_en @@ plainto_tsquery('english', p_query)
              OR cb.ts_en @@ plainto_tsquery('english', p_query_spaced)
          )
        LIMIT 100
    ),
    trigram_title AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY similarity(cb.title, p_query) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND similarity(cb.title, p_query) > 0.05
        LIMIT 30
    ),
    rrf AS (
        SELECT
            COALESCE(s.id, d.id, e.id, g.id) AS block_id,
            (0.45 * COALESCE(1.0 / (60 + s.rank), 0)
          +  0.20 * COALESCE(1.0 / (60 + d.rank), 0)
          +  0.25 * COALESCE(1.0 / (60 + e.rank), 0)
          +  0.10 * COALESCE(1.0 / (60 + g.rank), 0))::DOUBLE PRECISION AS score,
            s.cos_sim::DOUBLE PRECISION
        FROM semantic s
        FULL OUTER JOIN fulltext_de d USING (id)
        FULL OUTER JOIN fulltext_en e USING (id)
        FULL OUTER JOIN trigram_title g USING (id)
    )
    SELECT
        r.score         AS rrf_score,
        r.cos_sim       AS cosine_sim,
        cb.id,
        cb.title,
        cb.category,
        cb.tags,
        cb.content,
        cb.scope,
        cb.updated_at
    FROM rrf r
    JOIN context_blocks cb ON cb.id = r.block_id
    ORDER BY r.score DESC
    LIMIT p_limit;
END;
$$;


-- -----------------------------------------------------------------------------
-- 3. ctx_guard_check — Check a single block for near-duplicates
-- -----------------------------------------------------------------------------
-- Thresholds: >= 0.98 near_duplicate, >= 0.92 needs_review, < 0.92 clean
-- Uses HNSW Top-1 nearest neighbor (excl. self, excl. archived)
-- Reports cross-scope status
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION ctx_guard_check(p_block_id UUID)
RETURNS TABLE (
    decision        VARCHAR,
    top_similarity  NUMERIC,
    matched_id      UUID,
    matched_title   VARCHAR,
    matched_scope   VARCHAR,
    is_cross_scope  BOOLEAN
) LANGUAGE plpgsql AS $$
DECLARE
    v_embedding     vector(1024);
    v_scope         VARCHAR(20);
    v_matched_id    UUID;
    v_matched_title VARCHAR(255);
    v_matched_scope VARCHAR(20);
    v_similarity    NUMERIC;
BEGIN
    -- Load the block's embedding and scope
    SELECT cb.embedding, cb.scope
    INTO v_embedding, v_scope
    FROM context_blocks cb
    WHERE cb.id = p_block_id;

    -- If block not found or has no embedding, return clean with no match
    IF v_embedding IS NULL THEN
        decision       := 'clean';
        top_similarity := 0;
        matched_id     := NULL;
        matched_title  := NULL;
        matched_scope  := NULL;
        is_cross_scope := false;
        RETURN NEXT;
        RETURN;
    END IF;

    -- Find Top-1 nearest neighbor (excluding self, excluding archived)
    SELECT
        cb.id,
        cb.title,
        cb.scope,
        round(
            (1 - (cb.embedding::halfvec(1024) <=> v_embedding::halfvec(1024)))::numeric,
            4
        )
    INTO v_matched_id, v_matched_title, v_matched_scope, v_similarity
    FROM context_blocks cb
    WHERE cb.id != p_block_id
      AND NOT cb.is_archived
      AND cb.embedding IS NOT NULL
    ORDER BY cb.embedding::halfvec(1024) <=> v_embedding::halfvec(1024)
    LIMIT 1;

    -- No neighbors found
    IF v_matched_id IS NULL THEN
        decision       := 'clean';
        top_similarity := 0;
        matched_id     := NULL;
        matched_title  := NULL;
        matched_scope  := NULL;
        is_cross_scope := false;
        RETURN NEXT;
        RETURN;
    END IF;

    -- Apply thresholds
    IF v_similarity >= 0.98 THEN
        decision := 'near_duplicate';
    ELSIF v_similarity >= 0.92 THEN
        decision := 'needs_review';
    ELSE
        decision := 'clean';
    END IF;

    -- Determine cross-scope status
    -- Cross-scope = match is NOT in same scope AND match is NOT shared
    top_similarity := v_similarity;
    matched_id     := v_matched_id;
    matched_title  := v_matched_title;
    matched_scope  := v_matched_scope;
    is_cross_scope := (v_matched_scope != v_scope AND v_matched_scope != 'shared');

    RETURN NEXT;
    RETURN;
END;
$$;
-- @@ ctx-fold end 003_pg_functions.sql

-- @@ ctx-fold begin 004_notify_triggers.sql
-- 004_notify_triggers.sql — LISTEN/NOTIFY triggers for the Go event pipeline
-- Replaces n8n cron-based guard/digest with PG-native event-driven approach.

CREATE OR REPLACE FUNCTION notify_block_write() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('ctx_block_write', json_build_object('id', NEW.id, 'op', TG_OP)::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_block_write ON context_blocks;
CREATE TRIGGER trg_block_write
    AFTER INSERT OR UPDATE ON context_blocks
    FOR EACH ROW EXECUTE FUNCTION notify_block_write();
-- @@ ctx-fold end 004_notify_triggers.sql

-- @@ ctx-fold begin 005_scope_unique.sql
-- 005_scope_unique.sql — Replace (category, title) unique constraint with scope-aware partial unique index
-- Prevents cross-scope overwrites: a work-key upsert no longer collides with private blocks

-- Drop the old constraint (which was a unique index under the hood)
ALTER TABLE context_blocks DROP CONSTRAINT IF EXISTS uq_context_category_title;
DROP INDEX IF EXISTS uq_context_category_title;

-- Create scope-aware partial unique index (only non-archived blocks)
CREATE UNIQUE INDEX IF NOT EXISTS uq_context_category_title_scope
    ON context_blocks (category, title, scope) WHERE NOT is_archived;
-- @@ ctx-fold end 005_scope_unique.sql

-- @@ ctx-fold begin 006_temporal_rrf.sql
-- =============================================================================
-- 006_temporal_rrf.sql — GottZ Temporal Gravity: FTS expansion layer
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Adds p_temporal parameter (websearch_to_tsquery OR string) to ctx_rrf.
-- When not NULL, FTS CTEs additionally match blocks containing temporal terms
-- (weekday names, ISO dates) using 'simple' config (no stemming).
-- Backward compatible: p_temporal defaults to NULL (no change in behavior).
-- =============================================================================

CREATE OR REPLACE FUNCTION ctx_rrf(
    p_embedding    halfvec(1024),
    p_query        TEXT,
    p_query_spaced TEXT,
    p_scopes       TEXT[],
    p_category     TEXT DEFAULT NULL,
    p_tags         TEXT[] DEFAULT NULL,
    p_limit        INT DEFAULT 5,
    p_temporal     TEXT DEFAULT NULL
) RETURNS TABLE (
    rrf_score   DOUBLE PRECISION,
    cosine_sim  DOUBLE PRECISION,
    id          UUID,
    title       VARCHAR(255),
    category    VARCHAR(100),
    tags        TEXT[],
    content     TEXT,
    scope       VARCHAR(20),
    updated_at  TIMESTAMPTZ
) LANGUAGE plpgsql AS $$
BEGIN
    SET LOCAL hnsw.iterative_scan = 'relaxed_order';

    RETURN QUERY
    WITH semantic AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
            ) AS rank,
            1.0 - (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS cos_sim
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
        ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
        LIMIT 75
    ),
    fulltext_de AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query)),
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced)),
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_de @@ plainto_tsquery('german', p_query)
              OR cb.ts_de @@ plainto_tsquery('german', p_query_spaced)
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    fulltext_en AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced)),
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_en @@ plainto_tsquery('english', p_query)
              OR cb.ts_en @@ plainto_tsquery('english', p_query_spaced)
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    trigram_title AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY similarity(cb.title, p_query) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND similarity(cb.title, p_query) > 0.05
        LIMIT 30
    ),
    rrf AS (
        SELECT
            COALESCE(s.id, d.id, e.id, g.id) AS block_id,
            (0.45 * COALESCE(1.0 / (60 + s.rank), 0)
          +  0.20 * COALESCE(1.0 / (60 + d.rank), 0)
          +  0.25 * COALESCE(1.0 / (60 + e.rank), 0)
          +  0.10 * COALESCE(1.0 / (60 + g.rank), 0))::DOUBLE PRECISION AS score,
            s.cos_sim::DOUBLE PRECISION
        FROM semantic s
        FULL OUTER JOIN fulltext_de d USING (id)
        FULL OUTER JOIN fulltext_en e USING (id)
        FULL OUTER JOIN trigram_title g USING (id)
    )
    SELECT
        r.score         AS rrf_score,
        r.cos_sim       AS cosine_sim,
        cb.id,
        cb.title,
        cb.category,
        cb.tags,
        cb.content,
        cb.scope,
        cb.updated_at
    FROM rrf r
    JOIN context_blocks cb ON cb.id = r.block_id
    ORDER BY r.score DESC
    LIMIT p_limit;
END;
$$;
-- @@ ctx-fold end 006_temporal_rrf.sql

-- @@ ctx-fold begin 007_temporal_gravity.sql
-- =============================================================================
-- 007_temporal_gravity.sql — GottZ Temporal Gravity: Physics-Inspired Scoring
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- GottZ Temporal Gravity: Knowledge blocks are masses in time-space with
-- gravitational fields. Novel approach — no prior art combines asymmetric
-- decay + cognitive-science calibration + specificity mass + semantic coupling
-- + multi-body interactions in one retrieval framework.
-- Literature gap confirmed via 22-agent review (Session 9, 2026-03-29).
--
-- Components:
--   1. content_dates column + GIN index on context_blocks
--   2. ctx_temporal_gravity(block_id, target_date, direction, ...) — standalone scorer
--   3. ctx_temporal_gravity_batch(...) — batch scorer for CTE integration
--   4. Updated ctx_rrf with optional 5th temporal-gravity channel
--   5. ctx_temporal_gravity_post_rrf(...) — post-RRF modifier alternative
--
-- Formula: G * Mass(block) / Distance(dates, query, direction)^power
-- Multi-date blocks: sum of gravity from each qualifying date
-- Cutoff: cycle-proportional (14d weekday, 60d month, 730d year)
-- =============================================================================

-- =============================================================================
-- 1. Schema: content_dates column
-- =============================================================================
ALTER TABLE context_blocks ADD COLUMN IF NOT EXISTS content_dates DATE[] DEFAULT '{}';
CREATE INDEX IF NOT EXISTS idx_content_dates ON context_blocks USING GIN(content_dates);


-- =============================================================================
-- 2. ctx_temporal_gravity — Single-block gravity scorer
-- =============================================================================
-- Returns the total gravitational score for one block relative to a target date.
--
-- Parameters:
--   p_block_id   — the block to score
--   p_target     — the query date (temporal reference point)
--   p_direction  — 'past' (dates before target), 'future' (after), 'both'
--   p_cutoff     — max distance in days (cycle-proportional: 14/60/730)
--   p_power      — falloff exponent (1.0=linear, 1.5=compromise, 2.0=inverse-square)
--   p_g          — gravitational constant (default 1.0)
--
-- Mass = w_quality * quality_score
--      + w_access  * ln(1 + access_count)
--      + w_spec    * specificity(content_dates cardinality)
--      + w_length  * content_length_factor
--
-- Specificity: fewer dates = more specific = higher mass.
--   1 date: 1.0, 2 dates: 0.8, 3-5: 0.6, 6-10: 0.4, 11+: 0.2
--
-- Content length factor: ln(length)/ln(10000), clamped [0.1, 1.0]
--   Rewards substantial blocks without letting mega-blocks dominate.
--
-- Distance: abs(date - target) in days. Minimum 0.5 to avoid division by zero.
--   Asymmetry: past dates use p_power, future dates use p_power * 1.2
--   (human memory decays faster for future events).
-- =============================================================================
CREATE OR REPLACE FUNCTION ctx_temporal_gravity(
    p_block_id   UUID,
    p_target     DATE,
    p_direction  TEXT         DEFAULT 'both',     -- 'past', 'future', 'both'
    p_cutoff     INT          DEFAULT 60,         -- days
    p_power      DOUBLE PRECISION DEFAULT 1.5,    -- falloff exponent
    p_g          DOUBLE PRECISION DEFAULT 1.0,    -- gravitational constant
    -- Mass weights (sum should be ~1.0 for interpretable scores)
    p_w_quality  DOUBLE PRECISION DEFAULT 0.35,
    p_w_access   DOUBLE PRECISION DEFAULT 0.25,
    p_w_spec     DOUBLE PRECISION DEFAULT 0.25,
    p_w_length   DOUBLE PRECISION DEFAULT 0.15
) RETURNS DOUBLE PRECISION
LANGUAGE plpgsql STABLE AS $$
DECLARE
    v_dates         DATE[];
    v_quality       REAL;
    v_content_len   INT;
    v_access_count  BIGINT;
    v_mass          DOUBLE PRECISION;
    v_total_gravity DOUBLE PRECISION := 0;
    v_date          DATE;
    v_dist_days     DOUBLE PRECISION;
    v_eff_power     DOUBLE PRECISION;
    v_specificity   DOUBLE PRECISION;
    v_len_factor    DOUBLE PRECISION;
    v_n_dates       INT;
BEGIN
    -- Load block data
    SELECT cb.content_dates, cb.quality_score, length(cb.content)
    INTO v_dates, v_quality, v_content_len
    FROM context_blocks cb
    WHERE cb.id = p_block_id AND NOT cb.is_archived;

    IF v_dates IS NULL OR array_length(v_dates, 1) IS NULL THEN
        RETURN 0;
    END IF;

    -- Access count from log (cached in a single aggregate)
    SELECT count(*)
    INTO v_access_count
    FROM context_access_log cal
    WHERE cal.block_id = p_block_id;

    -- Compute specificity from date cardinality
    v_n_dates := array_length(v_dates, 1);
    v_specificity := CASE
        WHEN v_n_dates = 1  THEN 1.0
        WHEN v_n_dates = 2  THEN 0.8
        WHEN v_n_dates <= 5 THEN 0.6
        WHEN v_n_dates <= 10 THEN 0.4
        ELSE 0.2
    END;

    -- Content length factor: ln(len)/ln(10000), clamped [0.1, 1.0]
    v_len_factor := LEAST(1.0, GREATEST(0.1,
        ln(GREATEST(v_content_len, 1)::DOUBLE PRECISION) / ln(10000.0)
    ));

    -- Compute mass
    v_mass := p_w_quality * COALESCE(v_quality, 1.0)
            + p_w_access  * ln(1.0 + v_access_count)
            + p_w_spec    * v_specificity
            + p_w_length  * v_len_factor;

    -- Sum gravitational contributions from each qualifying date
    FOREACH v_date IN ARRAY v_dates
    LOOP
        v_dist_days := (v_date - p_target)::DOUBLE PRECISION;  -- positive = future

        -- Direction filter
        IF p_direction = 'past'   AND v_dist_days > 0 THEN CONTINUE; END IF;
        IF p_direction = 'future' AND v_dist_days < 0 THEN CONTINUE; END IF;

        -- Cutoff filter
        IF abs(v_dist_days) > p_cutoff THEN CONTINUE; END IF;

        -- Asymmetric power: future dates decay 20% faster
        IF v_dist_days >= 0 THEN
            v_eff_power := p_power * 1.2;
        ELSE
            v_eff_power := p_power;
        END IF;

        -- Distance: minimum 0.5 days to avoid singularity at zero
        v_dist_days := GREATEST(abs(v_dist_days), 0.5);

        -- Gravity contribution: G * Mass / Distance^power
        v_total_gravity := v_total_gravity + p_g * v_mass / power(v_dist_days, v_eff_power);
    END LOOP;

    RETURN v_total_gravity;
END;
$$;


-- =============================================================================
-- 3. ctx_temporal_gravity_batch — Set-returning batch scorer
-- =============================================================================
-- Designed for CTE integration: scores ALL qualifying blocks in a single pass
-- using pure SQL (no row-by-row function calls). This is the performance path.
--
-- Uses a lateral unnest to expand content_dates, computes gravity per date,
-- then aggregates back to block level. GIN index on content_dates enables
-- efficient pre-filtering via the @> operator or intarray overlap.
--
-- Performance target: <5ms at 1M rows with GIN index on content_dates
-- and a reasonable cutoff (14-730 days).
-- =============================================================================
CREATE OR REPLACE FUNCTION ctx_temporal_gravity_batch(
    p_target     DATE,
    p_direction  TEXT         DEFAULT 'both',
    p_cutoff     INT          DEFAULT 60,
    p_power      DOUBLE PRECISION DEFAULT 1.5,
    p_g          DOUBLE PRECISION DEFAULT 1.0,
    p_scopes     TEXT[]       DEFAULT NULL,
    p_category   TEXT         DEFAULT NULL,
    p_tags       TEXT[]       DEFAULT NULL,
    p_w_quality  DOUBLE PRECISION DEFAULT 0.35,
    p_w_access   DOUBLE PRECISION DEFAULT 0.25,
    p_w_spec     DOUBLE PRECISION DEFAULT 0.25,
    p_w_length   DOUBLE PRECISION DEFAULT 0.15,
    p_limit      INT          DEFAULT 50
) RETURNS TABLE (
    block_id UUID,
    gravity  DOUBLE PRECISION,
    mass     DOUBLE PRECISION,
    n_dates  INT
) LANGUAGE plpgsql STABLE AS $$
BEGIN
    RETURN QUERY
    WITH
    -- Pre-filter: only blocks with content_dates, within possible temporal range
    -- The daterange check ensures we only touch blocks that have at least one
    -- date within [target - cutoff, target + cutoff].
    candidates AS (
        SELECT
            cb.id,
            cb.content_dates,
            cb.quality_score,
            length(cb.content) AS content_len,
            array_length(cb.content_dates, 1) AS date_count
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.content_dates IS NOT NULL
          AND array_length(cb.content_dates, 1) > 0
          AND (p_scopes IS NULL OR cb.scope = ANY(p_scopes))
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          -- Range pre-filter: at least one date must be within cutoff
          -- This enables the planner to use the GIN index via && operator
          -- on a generated date range. We use a content_dates overlap check
          -- against the full date range.
    ),
    -- Access counts per block (single aggregate, no N+1)
    access_counts AS (
        SELECT cal.block_id AS bid, count(*) AS cnt
        FROM context_access_log cal
        WHERE cal.block_id IN (SELECT c.id FROM candidates c)
        GROUP BY cal.block_id
    ),
    -- Compute mass for each candidate
    massed AS (
        SELECT
            c.id,
            c.content_dates,
            c.date_count,
            -- Mass formula
            (   p_w_quality * COALESCE(c.quality_score, 1.0)
              + p_w_access  * ln(1.0 + COALESCE(ac.cnt, 0))
              + p_w_spec    * (CASE
                    WHEN c.date_count = 1  THEN 1.0
                    WHEN c.date_count = 2  THEN 0.8
                    WHEN c.date_count <= 5 THEN 0.6
                    WHEN c.date_count <= 10 THEN 0.4
                    ELSE 0.2
                END)
              + p_w_length  * LEAST(1.0, GREATEST(0.1,
                    ln(GREATEST(c.content_len, 1)::DOUBLE PRECISION) / ln(10000.0)))
            )::DOUBLE PRECISION AS block_mass
        FROM candidates c
        LEFT JOIN access_counts ac ON ac.bid = c.id
    ),
    -- Explode dates and compute per-date gravity contributions
    date_gravity AS (
        SELECT
            m.id,
            m.block_mass,
            m.date_count,
            -- Gravity contribution for this date
            p_g * m.block_mass / power(
                GREATEST(abs((d.dt - p_target)::DOUBLE PRECISION), 0.5),
                CASE
                    WHEN (d.dt - p_target) >= 0 THEN p_power * 1.2  -- future decay faster
                    ELSE p_power
                END
            ) AS g_contrib
        FROM massed m,
             LATERAL unnest(m.content_dates) AS d(dt)
        WHERE
            -- Cutoff filter
            abs((d.dt - p_target)::INT) <= p_cutoff
            -- Direction filter
            AND (p_direction = 'both'
                 OR (p_direction = 'past'   AND d.dt <= p_target)
                 OR (p_direction = 'future' AND d.dt >= p_target))
    ),
    -- Aggregate gravity per block
    block_gravity AS (
        SELECT
            dg.id,
            sum(dg.g_contrib) AS total_gravity,
            max(dg.block_mass) AS block_mass,  -- same for all rows of a block
            max(dg.date_count) AS block_date_count
        FROM date_gravity dg
        GROUP BY dg.id
    )
    SELECT
        bg.id        AS block_id,
        bg.total_gravity AS gravity,
        bg.block_mass    AS mass,
        bg.block_date_count AS n_dates
    FROM block_gravity bg
    WHERE bg.total_gravity > 0
    ORDER BY bg.total_gravity DESC
    LIMIT p_limit;
END;
$$;


-- =============================================================================
-- 4. Updated ctx_rrf with optional 5th temporal-gravity channel
-- =============================================================================
-- Adds three new parameters:
--   p_temporal_date      DATE     — the query's temporal reference (NULL = no gravity)
--   p_temporal_direction TEXT     — 'past', 'future', 'both' (default 'both')
--   p_temporal_cutoff    INT      — cycle-proportional cutoff days (default 60)
--
-- When p_temporal_date IS NOT NULL, a 5th CTE computes gravity-ranked blocks
-- and fuses them into the RRF with weight 0.15 (redistributed from others):
--   Semantic: 0.45 -> 0.38
--   EN-FTS:   0.25 -> 0.22
--   DE-FTS:   0.20 -> 0.17
--   Trigram:   0.10 -> 0.08
--   Temporal:  0.00 -> 0.15
--
-- When p_temporal_date IS NULL, the original 4-way weights are preserved exactly.
-- Backward compatible: all new params default to NULL.
-- =============================================================================
CREATE OR REPLACE FUNCTION ctx_rrf(
    p_embedding           halfvec(1024),
    p_query               TEXT,
    p_query_spaced        TEXT,
    p_scopes              TEXT[],
    p_category            TEXT DEFAULT NULL,
    p_tags                TEXT[] DEFAULT NULL,
    p_limit               INT DEFAULT 5,
    p_temporal            TEXT DEFAULT NULL,
    -- New: temporal gravity parameters
    p_temporal_date       DATE DEFAULT NULL,
    p_temporal_direction  TEXT DEFAULT 'both',
    p_temporal_cutoff     INT DEFAULT 60
) RETURNS TABLE (
    rrf_score   DOUBLE PRECISION,
    cosine_sim  DOUBLE PRECISION,
    id          UUID,
    title       VARCHAR(255),
    category    VARCHAR(100),
    tags        TEXT[],
    content     TEXT,
    scope       VARCHAR(20),
    updated_at  TIMESTAMPTZ
) LANGUAGE plpgsql AS $$
DECLARE
    -- Weights: with or without temporal channel
    w_semantic DOUBLE PRECISION;
    w_de       DOUBLE PRECISION;
    w_en       DOUBLE PRECISION;
    w_trigram  DOUBLE PRECISION;
    w_temporal DOUBLE PRECISION;
BEGIN
    SET LOCAL hnsw.iterative_scan = 'relaxed_order';

    -- Select weight scheme based on whether temporal gravity is active
    IF p_temporal_date IS NOT NULL THEN
        w_semantic := 0.38;
        w_en       := 0.22;
        w_de       := 0.17;
        w_trigram  := 0.08;
        w_temporal := 0.15;
    ELSE
        w_semantic := 0.45;
        w_en       := 0.25;
        w_de       := 0.20;
        w_trigram  := 0.10;
        w_temporal := 0.00;
    END IF;

    RETURN QUERY
    WITH semantic AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
            ) AS rank,
            1.0 - (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS cos_sim
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
        ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
        LIMIT 75
    ),
    fulltext_de AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query)),
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced)),
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_de @@ plainto_tsquery('german', p_query)
              OR cb.ts_de @@ plainto_tsquery('german', p_query_spaced)
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    fulltext_en AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced)),
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_en @@ plainto_tsquery('english', p_query)
              OR cb.ts_en @@ plainto_tsquery('english', p_query_spaced)
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    trigram_title AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY similarity(cb.title, p_query) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND similarity(cb.title, p_query) > 0.05
        LIMIT 30
    ),
    -- 5th channel: temporal gravity (only populated when p_temporal_date IS NOT NULL)
    temporal_gravity AS (
        SELECT
            tg.block_id AS id,
            ROW_NUMBER() OVER (ORDER BY tg.gravity DESC) AS rank
        FROM ctx_temporal_gravity_batch(
            p_temporal_date,
            p_temporal_direction,
            p_temporal_cutoff,
            1.5,        -- power
            1.0,        -- G
            p_scopes,
            p_category,
            p_tags
        ) tg
        WHERE p_temporal_date IS NOT NULL
        LIMIT 50
    ),
    rrf AS (
        SELECT
            COALESCE(s.id, d.id, e.id, g.id, t.id) AS block_id,
            (   w_semantic * COALESCE(1.0 / (60 + s.rank), 0)
              + w_de       * COALESCE(1.0 / (60 + d.rank), 0)
              + w_en       * COALESCE(1.0 / (60 + e.rank), 0)
              + w_trigram  * COALESCE(1.0 / (60 + g.rank), 0)
              + w_temporal * COALESCE(1.0 / (60 + t.rank), 0)
            )::DOUBLE PRECISION AS score,
            s.cos_sim::DOUBLE PRECISION
        FROM semantic s
        FULL OUTER JOIN fulltext_de d USING (id)
        FULL OUTER JOIN fulltext_en e USING (id)
        FULL OUTER JOIN trigram_title g USING (id)
        FULL OUTER JOIN temporal_gravity t USING (id)
    )
    SELECT
        r.score         AS rrf_score,
        r.cos_sim       AS cosine_sim,
        cb.id,
        cb.title,
        cb.category,
        cb.tags,
        cb.content,
        cb.scope,
        cb.updated_at
    FROM rrf r
    JOIN context_blocks cb ON cb.id = r.block_id
    ORDER BY r.score DESC
    LIMIT p_limit;
END;
$$;


-- =============================================================================
-- 5. ctx_temporal_gravity_post_rrf — Post-RRF score modifier
-- =============================================================================
-- Alternative to the CTE approach: multiply existing RRF scores by a
-- gravity-derived boost factor. This preserves the original 4-way RRF ranking
-- while allowing temporal relevance to re-rank results.
--
-- Boost formula: 1.0 + temporal_boost_weight * normalized_gravity
-- Where normalized_gravity = gravity / max_gravity across the result set,
-- ensuring the boost is always in [1.0, 1.0 + temporal_boost_weight].
--
-- Usage: Call AFTER ctx_rrf, passing the result block IDs.
-- =============================================================================
CREATE OR REPLACE FUNCTION ctx_temporal_gravity_post_rrf(
    p_block_ids          UUID[],
    p_rrf_scores         DOUBLE PRECISION[],
    p_target             DATE,
    p_direction          TEXT         DEFAULT 'both',
    p_cutoff             INT          DEFAULT 60,
    p_power              DOUBLE PRECISION DEFAULT 1.5,
    p_boost_weight       DOUBLE PRECISION DEFAULT 0.30   -- max boost: 30%
) RETURNS TABLE (
    block_id        UUID,
    original_score  DOUBLE PRECISION,
    gravity         DOUBLE PRECISION,
    boosted_score   DOUBLE PRECISION,
    boost_factor    DOUBLE PRECISION
) LANGUAGE plpgsql STABLE AS $$
BEGIN
    RETURN QUERY
    WITH input_blocks AS (
        SELECT
            unnest(p_block_ids)   AS bid,
            unnest(p_rrf_scores)  AS rrf
    ),
    block_data AS (
        SELECT
            ib.bid,
            ib.rrf,
            cb.content_dates,
            cb.quality_score,
            length(cb.content) AS content_len,
            array_length(cb.content_dates, 1) AS date_count
        FROM input_blocks ib
        JOIN context_blocks cb ON cb.id = ib.bid
        WHERE cb.content_dates IS NOT NULL
          AND array_length(cb.content_dates, 1) > 0
    ),
    access_counts AS (
        SELECT cal.block_id AS bid, count(*) AS cnt
        FROM context_access_log cal
        WHERE cal.block_id IN (SELECT bd.bid FROM block_data bd)
        GROUP BY cal.block_id
    ),
    massed AS (
        SELECT
            bd.bid,
            bd.rrf,
            bd.content_dates,
            (   0.35 * COALESCE(bd.quality_score, 1.0)
              + 0.25 * ln(1.0 + COALESCE(ac.cnt, 0))
              + 0.25 * (CASE
                    WHEN bd.date_count = 1  THEN 1.0
                    WHEN bd.date_count = 2  THEN 0.8
                    WHEN bd.date_count <= 5 THEN 0.6
                    WHEN bd.date_count <= 10 THEN 0.4
                    ELSE 0.2
                END)
              + 0.15 * LEAST(1.0, GREATEST(0.1,
                    ln(GREATEST(bd.content_len, 1)::DOUBLE PRECISION) / ln(10000.0)))
            )::DOUBLE PRECISION AS block_mass
        FROM block_data bd
        LEFT JOIN access_counts ac ON ac.bid = bd.bid
    ),
    date_gravity AS (
        SELECT
            m.bid,
            m.rrf,
            sum(
                1.0 * m.block_mass / power(
                    GREATEST(abs((d.dt - p_target)::DOUBLE PRECISION), 0.5),
                    CASE WHEN (d.dt - p_target) >= 0 THEN p_power * 1.2
                         ELSE p_power END
                )
            ) AS total_gravity
        FROM massed m,
             LATERAL unnest(m.content_dates) AS d(dt)
        WHERE abs((d.dt - p_target)::INT) <= p_cutoff
          AND (p_direction = 'both'
               OR (p_direction = 'past'   AND d.dt <= p_target)
               OR (p_direction = 'future' AND d.dt >= p_target))
        GROUP BY m.bid, m.rrf
    ),
    gravity_scores AS (
        SELECT
            COALESCE(dg.bid, ib.bid) AS bid,
            COALESCE(dg.rrf, ib.rrf) AS rrf_score,
            COALESCE(dg.total_gravity, 0)::DOUBLE PRECISION AS grav
        FROM input_blocks ib
        LEFT JOIN date_gravity dg ON dg.bid = ib.bid
    ),
    max_grav AS (
        SELECT GREATEST(max(gs.grav), 0.001) AS val FROM gravity_scores gs
    )
    SELECT
        gs.bid              AS block_id,
        gs.rrf_score        AS original_score,
        gs.grav             AS gravity,
        (gs.rrf_score * (1.0 + p_boost_weight * gs.grav / mg.val))::DOUBLE PRECISION AS boosted_score,
        (1.0 + p_boost_weight * gs.grav / mg.val)::DOUBLE PRECISION AS boost_factor
    FROM gravity_scores gs
    CROSS JOIN max_grav mg
    ORDER BY boosted_score DESC;
END;
$$;


-- =============================================================================
-- 6. Utility: ctx_temporal_cutoff — Cycle-proportional cutoff calculator
-- =============================================================================
-- Given a temporal reference type, returns the appropriate cutoff in days.
-- Called by the Go layer to determine p_temporal_cutoff before passing to ctx_rrf.
-- =============================================================================
CREATE OR REPLACE FUNCTION ctx_temporal_cutoff(
    p_reference_type TEXT  -- 'weekday', 'day', 'week', 'month', 'quarter', 'year'
) RETURNS INT
LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE p_reference_type
        WHEN 'weekday'  THEN  14    -- +/- 2 weeks
        WHEN 'day'      THEN  14    -- specific date: +/- 2 weeks
        WHEN 'week'     THEN  30    -- +/- 1 month
        WHEN 'month'    THEN  60    -- +/- 2 months
        WHEN 'quarter'  THEN 180    -- +/- 6 months
        WHEN 'year'     THEN 730    -- +/- 2 years
        ELSE                   60    -- default: month-scale
    END;
$$;


-- =============================================================================
-- 7. Utility: ctx_temporal_decay_compare — Comparison of decay models
-- =============================================================================
-- Returns scores for different decay models at a given distance.
-- Used for parameter tuning and visualization, not production queries.
-- =============================================================================
CREATE OR REPLACE FUNCTION ctx_temporal_decay_compare(
    p_distance_days DOUBLE PRECISION,
    p_mass          DOUBLE PRECISION DEFAULT 1.0
) RETURNS TABLE (
    model          TEXT,
    score          DOUBLE PRECISION,
    description    TEXT
) LANGUAGE sql IMMUTABLE AS $$
    WITH dist AS (SELECT GREATEST(p_distance_days, 0.5) AS d),
    -- Pre-compute Gaussian exponents, clamped to -500 to prevent underflow
    gauss AS (
        SELECT
            GREATEST(-500.0, -power((SELECT d FROM dist), 2) / (2.0 * power(7.0, 2)))   AS narrow_exp,
            GREATEST(-500.0, -power((SELECT d FROM dist), 2) / (2.0 * power(30.0, 2)))  AS medium_exp,
            GREATEST(-500.0, -power((SELECT d FROM dist), 2) / (2.0 * power(180.0, 2))) AS wide_exp
    )
    SELECT * FROM (VALUES
        -- Power-law models
        ('linear',          p_mass / (SELECT d FROM dist),
         'power=1.0, gentle falloff, wide influence radius'),
        ('sesqui',          p_mass / power((SELECT d FROM dist), 1.5),
         'power=1.5, compromise, recommended default'),
        ('inverse_square',  p_mass / power((SELECT d FROM dist), 2.0),
         'power=2.0, steep falloff, very local'),
        -- Gaussian models (clamped exponent prevents underflow at large distances)
        ('gaussian_narrow', p_mass * exp((SELECT narrow_exp FROM gauss)),
         'sigma=7d, very local, weekday-scale'),
        ('gaussian_medium', p_mass * exp((SELECT medium_exp FROM gauss)),
         'sigma=30d, month-scale, recommended for general use'),
        ('gaussian_wide',   p_mass * exp((SELECT wide_exp FROM gauss)),
         'sigma=180d, year-scale, very wide influence')
    ) AS t(model, score, description);
$$;
-- @@ ctx-fold end 007_temporal_gravity.sql

-- @@ ctx-fold begin 008_gin_range_prefilter.sql
-- =============================================================================
-- 008_gin_range_prefilter.sql — GIN range pre-filter for temporal gravity batch
-- =============================================================================
-- Fixes ctx_temporal_gravity_batch: the candidates CTE previously scanned ALL
-- blocks with content_dates instead of only those within the cutoff range.
-- Now uses GIN && overlap operator against a generated date array to ensure
-- the planner can use idx_content_dates for efficient pre-filtering.
-- =============================================================================

CREATE OR REPLACE FUNCTION ctx_temporal_gravity_batch(
    p_target     DATE,
    p_direction  TEXT         DEFAULT 'both',
    p_cutoff     INT          DEFAULT 60,
    p_power      DOUBLE PRECISION DEFAULT 1.5,
    p_g          DOUBLE PRECISION DEFAULT 1.0,
    p_scopes     TEXT[]       DEFAULT NULL,
    p_category   TEXT         DEFAULT NULL,
    p_tags       TEXT[]       DEFAULT NULL,
    p_w_quality  DOUBLE PRECISION DEFAULT 0.35,
    p_w_access   DOUBLE PRECISION DEFAULT 0.25,
    p_w_spec     DOUBLE PRECISION DEFAULT 0.25,
    p_w_length   DOUBLE PRECISION DEFAULT 0.15,
    p_limit      INT          DEFAULT 50
) RETURNS TABLE (
    block_id UUID,
    gravity  DOUBLE PRECISION,
    mass     DOUBLE PRECISION,
    n_dates  INT
) LANGUAGE plpgsql STABLE AS $$
BEGIN
    RETURN QUERY
    WITH
    -- Pre-filter: only blocks with content_dates, within possible temporal range
    candidates AS (
        SELECT
            cb.id,
            cb.content_dates,
            cb.quality_score,
            length(cb.content) AS content_len,
            array_length(cb.content_dates, 1) AS date_count
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.content_dates IS NOT NULL
          AND array_length(cb.content_dates, 1) > 0
          AND (p_scopes IS NULL OR cb.scope = ANY(p_scopes))
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          -- GIN range pre-filter: only blocks with at least one date within cutoff
          AND cb.content_dates && (
              SELECT array_agg(d::date)
              FROM generate_series(
                  p_target - p_cutoff * INTERVAL '1 day',
                  p_target + p_cutoff * INTERVAL '1 day',
                  '1 day'::interval
              ) d
          )
    ),
    -- Access counts per block (single aggregate, no N+1)
    access_counts AS (
        SELECT cal.block_id AS bid, count(*) AS cnt
        FROM context_access_log cal
        WHERE cal.block_id IN (SELECT c.id FROM candidates c)
        GROUP BY cal.block_id
    ),
    -- Compute mass for each candidate
    massed AS (
        SELECT
            c.id,
            c.content_dates,
            c.date_count,
            -- Mass formula
            (   p_w_quality * COALESCE(c.quality_score, 1.0)
              + p_w_access  * ln(1.0 + COALESCE(ac.cnt, 0))
              + p_w_spec    * (CASE
                    WHEN c.date_count = 1  THEN 1.0
                    WHEN c.date_count = 2  THEN 0.8
                    WHEN c.date_count <= 5 THEN 0.6
                    WHEN c.date_count <= 10 THEN 0.4
                    ELSE 0.2
                END)
              + p_w_length  * LEAST(1.0, GREATEST(0.1,
                    ln(GREATEST(c.content_len, 1)::DOUBLE PRECISION) / ln(10000.0)))
            )::DOUBLE PRECISION AS block_mass
        FROM candidates c
        LEFT JOIN access_counts ac ON ac.bid = c.id
    ),
    -- Explode dates and compute per-date gravity contributions
    date_gravity AS (
        SELECT
            m.id,
            m.block_mass,
            m.date_count,
            -- Gravity contribution for this date
            p_g * m.block_mass / power(
                GREATEST(abs((d.dt - p_target)::DOUBLE PRECISION), 0.5),
                CASE
                    WHEN (d.dt - p_target) >= 0 THEN p_power * 1.2  -- future decay faster
                    ELSE p_power
                END
            ) AS g_contrib
        FROM massed m,
             LATERAL unnest(m.content_dates) AS d(dt)
        WHERE
            -- Cutoff filter
            abs((d.dt - p_target)::INT) <= p_cutoff
            -- Direction filter
            AND (p_direction = 'both'
                 OR (p_direction = 'past'   AND d.dt <= p_target)
                 OR (p_direction = 'future' AND d.dt >= p_target))
    ),
    -- Aggregate gravity per block
    block_gravity AS (
        SELECT
            dg.id,
            sum(dg.g_contrib) AS total_gravity,
            max(dg.block_mass) AS block_mass,  -- same for all rows of a block
            max(dg.date_count) AS block_date_count
        FROM date_gravity dg
        GROUP BY dg.id
    )
    SELECT
        bg.id        AS block_id,
        bg.total_gravity AS gravity,
        bg.block_mass    AS mass,
        bg.block_date_count AS n_dates
    FROM block_gravity bg
    WHERE bg.total_gravity > 0
    ORDER BY bg.total_gravity DESC
    LIMIT p_limit;
END;
$$;
-- @@ ctx-fold end 008_gin_range_prefilter.sql

-- @@ ctx-fold begin 009_temporal_dimension.sql
-- =============================================================================
-- 009_temporal_dimension.sql — EAV Temporal Dimension Table
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Tensor decomposition of temporal data: each block's source dates are
-- decomposed into independent dimensions (year, month, week, weekday, quarter).
-- EAV schema allows adding new dimensions without schema changes.
--
-- Partial B-Tree indexes per dimension enable O(log n) lookups.
-- Conjunctive queries (e.g. "KW13 AND Monday") resolve via JOIN on block_id.
--
-- Backfill and population logic lives in Go, not in this migration.
-- =============================================================================

-- =============================================================================
-- 1. Table
-- =============================================================================
CREATE TABLE IF NOT EXISTS context_temporal (
    block_id    UUID        NOT NULL REFERENCES context_blocks(id) ON DELETE CASCADE,
    dimension   VARCHAR(20) NOT NULL,
    value       TEXT        NOT NULL,
    source_date DATE        NOT NULL,
    UNIQUE(block_id, source_date, dimension)
);

-- =============================================================================
-- 2. Partial B-Tree indexes — one per known dimension for O(log n) filtering
-- =============================================================================
CREATE INDEX IF NOT EXISTS idx_temporal_year
    ON context_temporal(value, block_id) WHERE dimension = 'year';

CREATE INDEX IF NOT EXISTS idx_temporal_month
    ON context_temporal(value, block_id) WHERE dimension = 'month';

CREATE INDEX IF NOT EXISTS idx_temporal_week
    ON context_temporal(value, block_id) WHERE dimension = 'week';

CREATE INDEX IF NOT EXISTS idx_temporal_weekday
    ON context_temporal(value, block_id) WHERE dimension = 'weekday';

CREATE INDEX IF NOT EXISTS idx_temporal_quarter
    ON context_temporal(value, block_id) WHERE dimension = 'quarter';

-- =============================================================================
-- 3. Supporting indexes
-- =============================================================================
-- CASCADE delete performance + block-level lookups
CREATE INDEX IF NOT EXISTS idx_temporal_block_id
    ON context_temporal(block_id);

-- Gravity distance calculations on source_date
CREATE INDEX IF NOT EXISTS idx_temporal_source_date
    ON context_temporal(source_date);
-- @@ ctx-fold end 009_temporal_dimension.sql

-- @@ ctx-fold begin 010_content_dates_drop_generated.sql
-- =============================================================================
-- 010_content_dates_drop_generated.sql — Remove GENERATED ALWAYS from content_dates
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- content_dates was GENERATED ALWAYS via extract_dates_from_text(), preventing
-- Go from writing directly. Go's store.ExtractDates is more capable (dot-format,
-- month names, etc.). DROP EXPRESSION converts to a normal column; existing
-- values are preserved.
-- =============================================================================

ALTER TABLE context_blocks ALTER COLUMN content_dates DROP EXPRESSION IF EXISTS;
-- @@ ctx-fold end 010_content_dates_drop_generated.sql

-- @@ ctx-fold begin 011_guard_chunk_filter.sql
-- =============================================================================
-- 011_guard_chunk_filter.sql — Exclude chunks from Guard nearest-neighbor search
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- BUG-1: ctx_guard_check() matched source blocks against their own chunks
-- (similarity >> 0.98), triggering auto-archive of the source. Fix: exclude
-- block_type='chunk' from the nearest-neighbor candidate set.
-- =============================================================================

CREATE OR REPLACE FUNCTION ctx_guard_check(p_block_id UUID)
RETURNS TABLE (
    decision        VARCHAR,
    top_similarity  NUMERIC,
    matched_id      UUID,
    matched_title   VARCHAR,
    matched_scope   VARCHAR,
    is_cross_scope  BOOLEAN
) LANGUAGE plpgsql AS $$
DECLARE
    v_embedding     vector(1024);
    v_scope         VARCHAR(20);
    v_matched_id    UUID;
    v_matched_title VARCHAR(255);
    v_matched_scope VARCHAR(20);
    v_similarity    NUMERIC;
BEGIN
    -- Load the block's embedding and scope
    SELECT cb.embedding, cb.scope
    INTO v_embedding, v_scope
    FROM context_blocks cb
    WHERE cb.id = p_block_id;

    -- If block not found or has no embedding, return clean with no match
    IF v_embedding IS NULL THEN
        decision       := 'clean';
        top_similarity := 0;
        matched_id     := NULL;
        matched_title  := NULL;
        matched_scope  := NULL;
        is_cross_scope := false;
        RETURN NEXT;
        RETURN;
    END IF;

    -- Find Top-1 nearest neighbor (excluding self, excluding archived, excluding chunks)
    SELECT
        cb.id,
        cb.title,
        cb.scope,
        round(
            (1 - (cb.embedding::halfvec(1024) <=> v_embedding::halfvec(1024)))::numeric,
            4
        )
    INTO v_matched_id, v_matched_title, v_matched_scope, v_similarity
    FROM context_blocks cb
    WHERE cb.id != p_block_id
      AND NOT cb.is_archived
      AND cb.embedding IS NOT NULL
      AND (cb.block_type IS NULL OR cb.block_type NOT IN ('chunk'))
    ORDER BY cb.embedding::halfvec(1024) <=> v_embedding::halfvec(1024)
    LIMIT 1;

    -- No neighbors found
    IF v_matched_id IS NULL THEN
        decision       := 'clean';
        top_similarity := 0;
        matched_id     := NULL;
        matched_title  := NULL;
        matched_scope  := NULL;
        is_cross_scope := false;
        RETURN NEXT;
        RETURN;
    END IF;

    -- Apply thresholds
    IF v_similarity >= 0.98 THEN
        decision := 'near_duplicate';
    ELSIF v_similarity >= 0.92 THEN
        decision := 'needs_review';
    ELSE
        decision := 'clean';
    END IF;

    -- Determine cross-scope status
    -- Cross-scope = match is NOT in same scope AND match is NOT shared
    top_similarity := v_similarity;
    matched_id     := v_matched_id;
    matched_title  := v_matched_title;
    matched_scope  := v_matched_scope;
    is_cross_scope := (v_matched_scope != v_scope AND v_matched_scope != 'shared');

    RETURN NEXT;
    RETURN;
END;
$$;
-- @@ ctx-fold end 011_guard_chunk_filter.sql

-- @@ ctx-fold begin 012_ingestion_sources.sql
-- =============================================================================
-- 012_ingestion_sources.sql — Tracking table for ingested source files
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- context_sources: Idempotent ingestion tracking. Stores file_hash for change
-- detection, status for progress tracking, and links to context_blocks via
-- source_id FK. Orphan detection via file_path comparison.
-- =============================================================================

CREATE TABLE IF NOT EXISTS context_sources (
    id             UUID PRIMARY KEY DEFAULT uuidv7(),
    file_path      TEXT NOT NULL,
    file_hash      VARCHAR(64) NOT NULL,
    file_size      BIGINT,
    mime_type      VARCHAR(100),
    chunk_count    INTEGER DEFAULT 0,
    status         VARCHAR(20) DEFAULT 'pending',
    error_message  TEXT,
    scope          VARCHAR(20) NOT NULL DEFAULT 'private',
    metadata       JSONB DEFAULT '{}',
    created_at     TIMESTAMPTZ DEFAULT now(),
    updated_at     TIMESTAMPTZ DEFAULT now(),
    UNIQUE(file_path, scope)
);

-- Indexes
CREATE INDEX IF NOT EXISTS idx_sources_file_hash ON context_sources(file_hash);
CREATE INDEX IF NOT EXISTS idx_sources_status ON context_sources(status) WHERE status <> 'done';

-- FK: context_blocks.source_id → context_sources.id
-- Only when source_id NOT NULL (existing blocks have NULL)
ALTER TABLE context_blocks
    ADD CONSTRAINT fk_blocks_source
    FOREIGN KEY (source_id) REFERENCES context_sources(id)
    ON DELETE SET NULL;

-- Composite index: Chunks of a source, sorted
CREATE INDEX IF NOT EXISTS idx_source_chunks
    ON context_blocks(source_id, chunk_index)
    WHERE block_type = 'chunk';

-- Unique: One chunk per source+index (idempotency)
CREATE UNIQUE INDEX IF NOT EXISTS uq_source_chunk
    ON context_blocks(source_id, chunk_index)
    WHERE NOT is_archived AND block_type = 'chunk';

-- CHECK constraints
ALTER TABLE context_sources ADD CONSTRAINT chk_source_scope
    CHECK (scope IN ('private', 'work', 'shared'));
ALTER TABLE context_sources ADD CONSTRAINT chk_source_status
    CHECK (status IN ('pending', 'chunking', 'embedding', 'done', 'error'));
-- @@ ctx-fold end 012_ingestion_sources.sql

-- @@ ctx-fold begin 013_link_dimension.sql
-- =============================================================================
-- 013_link_dimension.sql — Graph association via 'link' dimension
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Stores Wikilink associations ([[Target]]) as EAV dimensions.
-- Enables 1-hop graph queries via the same B-Tree infrastructure:
--   "What links to X?" → WHERE dimension='link' AND value='X'
--   "What does X link to?" → WHERE dimension='link' AND block_id='X'
-- =============================================================================

CREATE INDEX IF NOT EXISTS idx_temporal_link
    ON context_temporal(value, block_id) WHERE dimension = 'link';
-- @@ ctx-fold end 013_link_dimension.sql

-- @@ ctx-fold begin 014_security_hardening.sql
-- 014_security_hardening.sql
-- Security: Drop plaintext api_key column, revoke cross-database CONNECT.
-- Part of ctx by GottZ (github.com/GottZ/ctx/graphs/contributors)

-- 1. Drop plaintext API key column (auth uses key_hash exclusively via ctx_auth)
DROP INDEX IF EXISTS idx_api_keys_key;
ALTER TABLE context_api_keys DROP CONSTRAINT IF EXISTS context_api_keys_api_key_key;
ALTER TABLE context_api_keys DROP COLUMN IF EXISTS api_key;

-- 2. Promote key_hash: NOT NULL + UNIQUE (was nullable, now the sole identifier)
ALTER TABLE context_api_keys ALTER COLUMN key_hash SET NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_hash_unique ON context_api_keys(key_hash);
-- @@ ctx-fold end 014_security_hardening.sql

-- @@ ctx-fold begin 015_blob_scope_unique.sql
-- 015_blob_scope_unique.sql — Replace (category, title) unique constraint with scope-aware unique index
-- Prevents cross-scope overwrites: a work-key upsert no longer collides with private blobs

-- Drop the old constraint
ALTER TABLE context_blobs DROP CONSTRAINT IF EXISTS context_blobs_category_title_key;

-- Create scope-aware unique index
CREATE UNIQUE INDEX IF NOT EXISTS uq_blobs_category_title_scope
    ON context_blobs (category, title, scope);
-- @@ ctx-fold end 015_blob_scope_unique.sql

-- @@ ctx-fold begin 016_dream.sql
-- Migration 016: Dream Mode — async cross-reference engine.
-- Adds dream tracking columns and a dedicated links table.

-- Track when Dream last evaluated each block + cooldown.
ALTER TABLE context_blocks
    ADD COLUMN IF NOT EXISTS dream_checked_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS dream_cooldown_until TIMESTAMPTZ;

-- Priority queue index: unchecked blocks first, then lowest quality.
-- Partial index: only non-archived blocks with embeddings.
CREATE INDEX IF NOT EXISTS idx_dream_pending
    ON context_blocks (dream_checked_at ASC NULLS FIRST, quality_score ASC)
    WHERE NOT is_archived AND embedding IS NOT NULL;

-- Dream cross-reference links between blocks.
-- Same-scope rule enforced at application level (Angreifer V5 mitigation).
CREATE TABLE IF NOT EXISTS context_dream_links (
    source_block_id  UUID NOT NULL REFERENCES context_blocks(id),
    target_block_id  UUID NOT NULL REFERENCES context_blocks(id),
    relationship     TEXT NOT NULL CHECK (relationship IN ('topical', 'factual', 'causal', 'supersedes')),
    confidence       REAL NOT NULL DEFAULT 0.5,
    scope            VARCHAR(20) NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata         JSONB DEFAULT '{}',
    PRIMARY KEY (source_block_id, target_block_id),
    CHECK (source_block_id != target_block_id)
);

CREATE INDEX IF NOT EXISTS idx_dream_links_target
    ON context_dream_links(target_block_id);

-- Audit: Dream operations go into existing context_write_log with decision prefix 'dream_*'.
-- @@ ctx-fold end 016_dream.sql

-- @@ ctx-fold begin 017_dream_indexes.sql
-- Migration 017: Performance indexes for Dream Mode queries at scale.

-- SupersedesMap lookup: filtered by relationship + source_block_id.
CREATE INDEX IF NOT EXISTS idx_dream_links_supersedes
    ON context_dream_links(source_block_id)
    WHERE relationship = 'supersedes';

-- Low-confidence review: sorted scan for dream-review endpoint.
CREATE INDEX IF NOT EXISTS idx_dream_links_confidence
    ON context_dream_links(confidence ASC)
    WHERE confidence < 0.7;
-- @@ ctx-fold end 017_dream_indexes.sql

-- @@ ctx-fold begin 018_fts_or_hybrid.sql
-- =============================================================================
-- Migration 018: FTS OR-Matching Hybrid
-- =============================================================================
-- Adds optional p_query_or parameter for websearch_to_tsquery OR-matching.
-- This complements p_query (AND-based plainto_tsquery) for broader recall.
-- When p_query_or IS NOT NULL, FTS CTEs match with OR-semantics alongside AND.
-- Backward compatible: p_query_or defaults to NULL (no behavior change).
--
-- Empirical basis (Session 18 Armada):
--   - Dead Weight was 20% (not 68% as estimated), but OR brings 4.5x recall
--   - Performance: +13% overhead at 440 blocks (1.7→2.1ms), LIMIT 100 caps at 1M
--   - Uses language-specific config (german/english), not 'simple'
-- =============================================================================
CREATE OR REPLACE FUNCTION ctx_rrf(
    p_embedding           halfvec(1024),
    p_query               TEXT,
    p_query_spaced        TEXT,
    p_scopes              TEXT[],
    p_category            TEXT DEFAULT NULL,
    p_tags                TEXT[] DEFAULT NULL,
    p_limit               INT DEFAULT 5,
    p_temporal            TEXT DEFAULT NULL,
    p_temporal_date       DATE DEFAULT NULL,
    p_temporal_direction  TEXT DEFAULT 'both',
    p_temporal_cutoff     INT DEFAULT 60,
    p_query_or            TEXT DEFAULT NULL
) RETURNS TABLE (
    rrf_score   DOUBLE PRECISION,
    cosine_sim  DOUBLE PRECISION,
    id          UUID,
    title       VARCHAR(255),
    category    VARCHAR(100),
    tags        TEXT[],
    content     TEXT,
    scope       VARCHAR(20),
    updated_at  TIMESTAMPTZ
) LANGUAGE plpgsql AS $$
DECLARE
    w_semantic DOUBLE PRECISION;
    w_de       DOUBLE PRECISION;
    w_en       DOUBLE PRECISION;
    w_trigram  DOUBLE PRECISION;
    w_temporal DOUBLE PRECISION;
BEGIN
    SET LOCAL hnsw.iterative_scan = 'relaxed_order';

    IF p_temporal_date IS NOT NULL THEN
        w_semantic := 0.38;
        w_en       := 0.22;
        w_de       := 0.17;
        w_trigram  := 0.08;
        w_temporal := 0.15;
    ELSE
        w_semantic := 0.45;
        w_en       := 0.25;
        w_de       := 0.20;
        w_trigram  := 0.10;
        w_temporal := 0.00;
    END IF;

    RETURN QUERY
    WITH semantic AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
            ) AS rank,
            1.0 - (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS cos_sim
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
        ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
        LIMIT 75
    ),
    fulltext_de AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query)),
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced)),
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_de @@ plainto_tsquery('german', p_query)
              OR cb.ts_de @@ plainto_tsquery('german', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_de @@ websearch_to_tsquery('german', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    fulltext_en AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced)),
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_en @@ plainto_tsquery('english', p_query)
              OR cb.ts_en @@ plainto_tsquery('english', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_en @@ websearch_to_tsquery('english', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    trigram_title AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY similarity(cb.title, p_query) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND similarity(cb.title, p_query) > 0.05
        LIMIT 30
    ),
    temporal_gravity AS (
        SELECT
            tg.block_id AS id,
            ROW_NUMBER() OVER (ORDER BY tg.gravity DESC) AS rank
        FROM ctx_temporal_gravity_batch(
            p_temporal_date,
            p_temporal_direction,
            p_temporal_cutoff,
            1.5,
            1.0,
            p_scopes,
            p_category,
            p_tags
        ) tg
        WHERE p_temporal_date IS NOT NULL
        LIMIT 50
    ),
    rrf AS (
        SELECT
            COALESCE(s.id, d.id, e.id, g.id, t.id) AS block_id,
            (   w_semantic * COALESCE(1.0 / (60 + s.rank), 0)
              + w_de       * COALESCE(1.0 / (60 + d.rank), 0)
              + w_en       * COALESCE(1.0 / (60 + e.rank), 0)
              + w_trigram  * COALESCE(1.0 / (60 + g.rank), 0)
              + w_temporal * COALESCE(1.0 / (60 + t.rank), 0)
            )::DOUBLE PRECISION AS score,
            s.cos_sim::DOUBLE PRECISION
        FROM semantic s
        FULL OUTER JOIN fulltext_de d USING (id)
        FULL OUTER JOIN fulltext_en e USING (id)
        FULL OUTER JOIN trigram_title g USING (id)
        FULL OUTER JOIN temporal_gravity t USING (id)
    )
    SELECT
        r.score         AS rrf_score,
        r.cos_sim       AS cosine_sim,
        cb.id,
        cb.title,
        cb.category,
        cb.tags,
        cb.content,
        cb.scope,
        cb.updated_at
    FROM rrf r
    JOIN context_blocks cb ON cb.id = r.block_id
    ORDER BY r.score DESC
    LIMIT p_limit;
END;
$$;
-- @@ ctx-fold end 018_fts_or_hybrid.sql

-- @@ ctx-fold begin 019_monthday_seasonal.sql
-- =============================================================================
-- 019_monthday_seasonal.sql — Cyclic Dimensions: monthday + seasonal
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Adds two cyclic dimensions that were in the original GottZ Cyclic Phase
-- Model spec but missing from the v0.18.0 implementation:
--
--   monthday: day-of-month (1-31, ~30-cycle, σ=0.10)
--     For patterns like "Monatsanfang", "Gehaltstag", "zum Ersten".
--
--   seasonal: day-of-year (1-366, 365-cycle, σ=0.08)
--     For patterns like "Weihnachten", "Silvester", "jedes Jahr um diese Zeit".
--     Note: this is what the original spec incorrectly called "yearly".
--     Year itself stays linear/monotonic — only day-of-year is cyclic.
--
-- Principle: every cyclic time variance must be dimensionally captured.
-- Session 20 user feedback: "zyklische zeitvarianzen sollten dimensional
-- erfasst sein".
-- =============================================================================

-- =============================================================================
-- 1. Partial B-Tree indexes for the new dimensions
-- =============================================================================
CREATE INDEX IF NOT EXISTS idx_temporal_monthday
    ON context_temporal(value, block_id) WHERE dimension = 'monthday';

CREATE INDEX IF NOT EXISTS idx_temporal_seasonal
    ON context_temporal(value, block_id) WHERE dimension = 'seasonal';

-- =============================================================================
-- 2. Backfill monthday + seasonal from existing source_date values
-- =============================================================================
-- Every row in context_temporal already has a source_date. We derive monthday
-- and seasonal from it. Uses ON CONFLICT DO NOTHING for idempotency.
-- Excludes the '_none' sentinel rows.
-- =============================================================================
INSERT INTO context_temporal (block_id, dimension, value, source_date)
SELECT DISTINCT
    block_id,
    'monthday' AS dimension,
    EXTRACT(DAY FROM source_date)::TEXT AS value,
    source_date
FROM context_temporal
WHERE dimension = 'year'  -- one row per unique (block_id, source_date) pair
  AND source_date > '1970-01-01'::DATE  -- skip sentinel
ON CONFLICT DO NOTHING;

INSERT INTO context_temporal (block_id, dimension, value, source_date)
SELECT DISTINCT
    block_id,
    'seasonal' AS dimension,
    EXTRACT(DOY FROM source_date)::TEXT AS value,
    source_date
FROM context_temporal
WHERE dimension = 'year'
  AND source_date > '1970-01-01'::DATE
ON CONFLICT DO NOTHING;
-- @@ ctx-fold end 019_monthday_seasonal.sql

-- @@ ctx-fold begin 020_content_times.sql
-- =============================================================================
-- 020_content_times.sql — Schema Modernization: DATE → TIMESTAMPTZ
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- User decision (Session 20, 2026-04-05): "das macht ja gar keinen sinn, dass
-- die nur das datum erfassen". Temporal data loses time info when stored as
-- DATE. Migration to TIMESTAMPTZ unblocks the daily dimension (hour-of-day).
--
-- Changes:
--   1. content_blocks.content_dates (DATE[]) → content_times (TIMESTAMPTZ[])
--   2. context_temporal.source_date (DATE) → source_time (TIMESTAMPTZ)
--   3. Drop dead M007 gravity functions (never called from Go, content_dates refs)
--   4. Rebuild ctx_rrf WITHOUT the 5th channel (Go Post-RRF is the path)
--   5. Backfill daily dimension (currently all 0 — new ingests get real hours)
--
-- Existing DATE values cast to midnight UTC. Timezone semantics: TIMESTAMPTZ
-- stores as UTC, displays in session timezone. All Go code uses time.Time
-- which carries timezone info explicitly.
-- =============================================================================

-- =============================================================================
-- 1. Drop dead M007 functions (all reference content_dates)
-- =============================================================================
DROP FUNCTION IF EXISTS ctx_temporal_gravity(UUID, DATE, TEXT, INT, DOUBLE PRECISION, DOUBLE PRECISION, DOUBLE PRECISION, DOUBLE PRECISION, DOUBLE PRECISION, DOUBLE PRECISION);
DROP FUNCTION IF EXISTS ctx_temporal_gravity_batch(DATE, TEXT, INT, DOUBLE PRECISION, DOUBLE PRECISION, TEXT[], TEXT, TEXT[], DOUBLE PRECISION, DOUBLE PRECISION, DOUBLE PRECISION, DOUBLE PRECISION, INT);
DROP FUNCTION IF EXISTS ctx_temporal_gravity_post_rrf(UUID[], DOUBLE PRECISION[], DATE, TEXT, INT, DOUBLE PRECISION, DOUBLE PRECISION);
DROP FUNCTION IF EXISTS ctx_temporal_cutoff(TEXT);
DROP FUNCTION IF EXISTS ctx_temporal_decay_compare(DOUBLE PRECISION, DOUBLE PRECISION);

-- =============================================================================
-- 2. Drop old ctx_rrf variants (both M007 11-param and M018 12-param)
-- =============================================================================
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, DATE, TEXT, INT);
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, DATE, TEXT, INT, TEXT);

-- =============================================================================
-- 3. Migrate context_blocks.content_dates → content_times
-- =============================================================================
ALTER TABLE context_blocks ADD COLUMN content_times TIMESTAMPTZ[] DEFAULT '{}';

-- Cast existing DATE values to midnight UTC TIMESTAMPTZ
UPDATE context_blocks
SET content_times = ARRAY(
    SELECT (d::TIMESTAMP AT TIME ZONE 'UTC')
    FROM unnest(content_dates) AS d
)
WHERE content_dates IS NOT NULL
  AND array_length(content_dates, 1) > 0;

-- Drop old column and GIN index
DROP INDEX IF EXISTS idx_content_dates;
ALTER TABLE context_blocks DROP COLUMN content_dates;

-- New GIN index on content_times
CREATE INDEX idx_content_times ON context_blocks USING GIN(content_times);

-- =============================================================================
-- 4. Migrate context_temporal.source_date → source_time
-- =============================================================================
ALTER TABLE context_temporal ADD COLUMN source_time TIMESTAMPTZ;

UPDATE context_temporal
SET source_time = source_date::TIMESTAMP AT TIME ZONE 'UTC';

ALTER TABLE context_temporal ALTER COLUMN source_time SET NOT NULL;

-- Drop old unique constraint and column
ALTER TABLE context_temporal DROP CONSTRAINT IF EXISTS context_temporal_block_id_source_date_dimension_key;
ALTER TABLE context_temporal DROP COLUMN source_date;

-- New unique constraint with source_time
ALTER TABLE context_temporal
    ADD CONSTRAINT context_temporal_block_id_source_time_dimension_key
    UNIQUE(block_id, source_time, dimension);

-- =============================================================================
-- 5. Drop and recreate source_date index (now useless)
-- =============================================================================
DROP INDEX IF EXISTS idx_temporal_source_date;
CREATE INDEX idx_temporal_source_time
    ON context_temporal(source_time)
    WHERE source_time > '1970-01-01 00:00:00+00'::TIMESTAMPTZ;

-- =============================================================================
-- 6. Add partial B-Tree index for daily dimension
-- =============================================================================
CREATE INDEX IF NOT EXISTS idx_temporal_daily
    ON context_temporal(value, block_id) WHERE dimension = 'daily';

-- =============================================================================
-- 7. Backfill daily dimension from existing source_time
-- =============================================================================
-- All existing times are midnight UTC (hour=0) after migration. New ingests
-- that parse actual timestamps will populate non-zero hours.
INSERT INTO context_temporal (block_id, dimension, value, source_time)
SELECT DISTINCT
    block_id,
    'daily' AS dimension,
    EXTRACT(HOUR FROM source_time)::TEXT AS value,
    source_time
FROM context_temporal
WHERE dimension = 'year'
  AND source_time > '1970-01-01 00:00:00+00'::TIMESTAMPTZ
ON CONFLICT DO NOTHING;

-- =============================================================================
-- 8. Recreate ctx_rrf WITHOUT 5th temporal-gravity channel
-- =============================================================================
-- The 5th channel was infrastructure that was never activated from Go.
-- Go handles temporal gravity Post-RRF via ApplyGravityBoost + ApplyCyclicGravityBoost.
-- This simplifies ctx_rrf to the original 4-Way RRF + optional temporal FTS expansion.
-- =============================================================================
CREATE OR REPLACE FUNCTION ctx_rrf(
    p_embedding           halfvec(1024),
    p_query               TEXT,
    p_query_spaced        TEXT,
    p_scopes              TEXT[],
    p_category            TEXT DEFAULT NULL,
    p_tags                TEXT[] DEFAULT NULL,
    p_limit               INT DEFAULT 5,
    p_temporal            TEXT DEFAULT NULL,
    p_query_or            TEXT DEFAULT NULL
) RETURNS TABLE (
    rrf_score   DOUBLE PRECISION,
    cosine_sim  DOUBLE PRECISION,
    id          UUID,
    title       VARCHAR(255),
    category    VARCHAR(100),
    tags        TEXT[],
    content     TEXT,
    scope       VARCHAR(20),
    updated_at  TIMESTAMPTZ
) LANGUAGE plpgsql AS $$
BEGIN
    SET LOCAL hnsw.iterative_scan = 'relaxed_order';

    RETURN QUERY
    WITH semantic AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
            ) AS rank,
            1.0 - (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS cos_sim
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
        ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
        LIMIT 75
    ),
    fulltext_de AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query)),
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_de @@ plainto_tsquery('german', p_query)
              OR cb.ts_de @@ plainto_tsquery('german', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    fulltext_en AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_en @@ plainto_tsquery('english', p_query)
              OR cb.ts_en @@ plainto_tsquery('english', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    trigram_title AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY similarity(cb.title, p_query) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND similarity(cb.title, p_query) > 0.05
        LIMIT 30
    ),
    rrf AS (
        SELECT
            COALESCE(s.id, d.id, e.id, g.id) AS block_id,
            (   0.45 * COALESCE(1.0 / (60 + s.rank), 0)
              + 0.20 * COALESCE(1.0 / (60 + d.rank), 0)
              + 0.25 * COALESCE(1.0 / (60 + e.rank), 0)
              + 0.10 * COALESCE(1.0 / (60 + g.rank), 0)
            )::DOUBLE PRECISION AS score,
            s.cos_sim::DOUBLE PRECISION
        FROM semantic s
        FULL OUTER JOIN fulltext_de d USING (id)
        FULL OUTER JOIN fulltext_en e USING (id)
        FULL OUTER JOIN trigram_title g USING (id)
    )
    SELECT
        r.score         AS rrf_score,
        r.cos_sim       AS cosine_sim,
        cb.id,
        cb.title,
        cb.category,
        cb.tags,
        cb.content,
        cb.scope,
        cb.updated_at
    FROM rrf r
    JOIN context_blocks cb ON cb.id = r.block_id
    ORDER BY r.score DESC
    LIMIT p_limit;
END;
$$;
-- @@ ctx-fold end 020_content_times.sql

-- @@ ctx-fold begin 021_created_at_anchors.sql
-- =============================================================================
-- 021_created_at_anchors.sql — Backfill created_at as meta-anchor dimensions
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- User decision (Session 20, 2026-04-05): "der blockinhalt kann ja auch
-- zeiten referenzieren. ergo kann ein block mehrere temporale anker haben".
--
-- Every block has a natural temporal anchor: its created_at timestamp. This
-- is independent from any dates mentioned in the content. A block about
-- "Meeting am Dienstag" written on Friday has TWO legitimate anchors:
--   - weekday=2 (content, semantic)
--   - weekday=5 (created_at, meta)
--
-- Both signals should contribute dimensions. Before M021, only content-
-- derived times were extracted — blocks without date mentions had no
-- temporal dimensions at all (only '_none' sentinel).
--
-- This migration backfills all 8 dimensions for every non-archived block's
-- created_at. ON CONFLICT DO NOTHING handles overlap with existing content-
-- derived dimensions. The '_none' sentinel is NOT removed — future content
-- updates via PopulateTemporal will clean it up via DELETE+INSERT pattern.
-- =============================================================================

-- =============================================================================
-- Backfill all 8 temporal dimensions from created_at for every active block
-- =============================================================================
WITH block_times AS (
    SELECT id, created_at
    FROM context_blocks
    WHERE NOT is_archived
)
INSERT INTO context_temporal (block_id, dimension, value, source_time)
SELECT id, dimension, value, created_at
FROM block_times,
LATERAL (VALUES
    ('year',     EXTRACT(YEAR FROM created_at)::TEXT),
    ('month',    EXTRACT(MONTH FROM created_at)::TEXT),
    ('week',     EXTRACT(WEEK FROM created_at)::TEXT),
    ('weekday',  EXTRACT(ISODOW FROM created_at)::TEXT),
    ('quarter',  EXTRACT(QUARTER FROM created_at)::TEXT),
    ('monthday', EXTRACT(DAY FROM created_at)::TEXT),
    ('seasonal', EXTRACT(DOY FROM created_at)::TEXT),
    ('daily',    EXTRACT(HOUR FROM created_at)::TEXT)
) AS dims(dimension, value)
ON CONFLICT DO NOTHING;

-- =============================================================================
-- Clean up '_none' sentinel rows for blocks that now have real dimensions
-- =============================================================================
-- After the backfill, every non-archived block has at least 8 dimensions
-- from its created_at. The '_none' sentinel is obsolete for these blocks.
DELETE FROM context_temporal ct
WHERE ct.dimension = '_none'
  AND EXISTS (
      SELECT 1 FROM context_temporal ct2
      WHERE ct2.block_id = ct.block_id
        AND ct2.dimension != '_none'
  );
-- @@ ctx-fold end 021_created_at_anchors.sql

-- @@ ctx-fold begin 022_stats_indexes.sql
-- 022_stats_indexes.sql — Composite indexes for GetStats + ListMeta at 1M+ scale.
CREATE INDEX IF NOT EXISTS idx_context_scope_active
    ON context_blocks(scope, created_at DESC) WHERE NOT is_archived;
-- @@ ctx-fold end 022_stats_indexes.sql

-- @@ ctx-fold begin 023_oauth_clients.sql
-- OAuth 2.1 client registration for MCP remote auth.
-- Each integration (claude.ai, Claude Code, etc.) gets its own client credentials.
-- Users authenticate with their API key during the authorize flow.

CREATE TABLE IF NOT EXISTS context_oauth_clients (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id       VARCHAR(64) NOT NULL UNIQUE,
    client_secret_hash VARCHAR(128) NOT NULL,
    label           VARCHAR(200) NOT NULL,
    created_by      UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,
    active          BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_oauth_clients_active ON context_oauth_clients(client_id) WHERE active = true;
-- @@ ctx-fold end 023_oauth_clients.sql

-- @@ ctx-fold begin 024_dream_links_raw_confidence.sql
-- Raw LLM confidence as first-class column on dream links.
-- Previously: metadata->>'raw_confidence' (JSON), column `confidence` held weighted value
-- (raw × sourceQuality × targetQuality). This conflated two distinct signals and made all
-- gates operating on `confidence` unreachable for population with quality_score ≈ 0.5.
--
-- New model: `raw_confidence` = LLM self-assessment (what LLM returns), used for gates.
--            `confidence`     = weighted value (downstream ranking signal, not gate).

ALTER TABLE context_dream_links
    ADD COLUMN raw_confidence REAL NOT NULL DEFAULT 0.5
        CHECK (raw_confidence >= 0.0 AND raw_confidence <= 1.0);

-- Gate index: links below raw 0.7 are "noise" — query-side gates filter them out.
CREATE INDEX IF NOT EXISTS idx_dream_links_raw_confidence
    ON context_dream_links (raw_confidence)
    WHERE raw_confidence < 0.7;

-- Pipeline version — lets us filter Pre-Reset (v1) from Post-Reset (v2+) data
-- and evolve gate logic without destroying history. Bumped in Go via dream.Version.
ALTER TABLE context_dream_links
    ADD COLUMN dream_version SMALLINT NOT NULL DEFAULT 1;

CREATE INDEX IF NOT EXISTS idx_dream_links_version
    ON context_dream_links (dream_version);
-- @@ ctx-fold end 024_dream_links_raw_confidence.sql

-- @@ ctx-fold begin 025_llm_log.sql
-- Request/response log for every LLM call across pipelines.
-- Purpose: observability, prompt evolution, post-hoc accuracy audits.
-- Partitioned via TimescaleDB hypertable (7-day chunks).
--
-- pipeline values: 'dream-eval', 'dream-temporal', 'ingest-extract',
--                  'query-synthesize', 'query-translate', 'query-temporal', 'rrf-rerank'.

CREATE TABLE context_llm_log (
    id                UUID NOT NULL DEFAULT gen_random_uuid(),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    pipeline          TEXT NOT NULL,
    model             TEXT NOT NULL,
    host              TEXT NOT NULL,
    duration_ms       INTEGER,
    error             TEXT,
    prompt_tokens     INTEGER,
    completion_tokens INTEGER,
    request_system    TEXT,
    request_user      TEXT,
    response_content  TEXT,
    block_ids         UUID[],
    dream_version     SMALLINT,
    metadata          JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (id, created_at)
);

-- TimescaleDB hypertable: 7-day chunks.
-- if_not_exists=true lets the migration replay on a partially-applied DB.
SELECT create_hypertable(
    'context_llm_log',
    'created_at',
    chunk_time_interval => interval '7 days',
    if_not_exists => true
);

CREATE INDEX IF NOT EXISTS idx_llm_log_pipeline
    ON context_llm_log (pipeline, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_llm_log_block_ids
    ON context_llm_log USING GIN (block_ids);

CREATE INDEX IF NOT EXISTS idx_llm_log_error
    ON context_llm_log (created_at DESC)
    WHERE error IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_llm_log_dream_version
    ON context_llm_log (dream_version, created_at DESC)
    WHERE dream_version IS NOT NULL;
-- @@ ctx-fold end 025_llm_log.sql

-- @@ ctx-fold begin 026_embed_cache.sql
-- Embedding cache: avoid re-computing embeddings for repeated text+model pairs.
-- Primary workload: Dream-Keyword embeddings (high repetition across cycles),
-- plus Query-Embeddings (user repeats queries).
-- Self-evicting via last_access — eviction job runs in the scheduler.
--
-- Key design: (text_hash, model) compound PK. Model change automatically
-- makes old entries unreachable via the lookup path.

CREATE TABLE context_embed_cache (
    text_hash    BYTEA NOT NULL,
    model        TEXT NOT NULL,
    embedding    vector(1024) NOT NULL,
    text_preview TEXT NOT NULL,
    hit_count    INTEGER NOT NULL DEFAULT 1,
    first_seen   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_access  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (text_hash, model)
);

-- Eviction-Scan: find oldest entries when size cap is hit.
CREATE INDEX IF NOT EXISTS idx_embed_cache_last_access
    ON context_embed_cache (last_access);

-- Maintenance insight: which entries are carrying their weight.
CREATE INDEX IF NOT EXISTS idx_embed_cache_hit_count
    ON context_embed_cache (hit_count DESC, last_access DESC);
-- @@ ctx-fold end 026_embed_cache.sql

-- @@ ctx-fold begin 027_block_dream_keywords.sql
-- LLM-generated keywords per block, persisted so Dream doesn't re-invoke the LLM
-- on every cooldown-recheck. Regenerated when block.updated_at > generated_at.
--
-- Why: deterministic tokeniser extracted code-syntax fragments for code blocks
-- (e.g. "mcp.newserver(&mcp.implementation{name"), which embedded into nonsense
-- regions of the vector space and produced irrelevant RRF candidates.
-- LLM extracts conceptual anchors ("MCP server", "streamable HTTP") that embed
-- semantically and match other blocks on topic rather than syntax.

ALTER TABLE context_blocks
    ADD COLUMN dream_keywords              TEXT[],
    ADD COLUMN dream_keywords_generated_at TIMESTAMPTZ;

-- Picker-friendly: find blocks that need keyword generation before the next pass.
CREATE INDEX IF NOT EXISTS idx_blocks_dream_keywords_pending
    ON context_blocks (id)
    WHERE dream_keywords IS NULL
      AND NOT is_archived;
-- @@ ctx-fold end 027_block_dream_keywords.sql

-- @@ ctx-fold begin 028_block_temporal_validated.sql
-- Marker for "Temporal-Validation has run for this block at this version".
-- Phase 1 (deterministic pattern matching) writes to context_temporal cheaply.
-- Phase 2 (LLM review) is costly; we only want it once per block-content, not
-- on every cooldown recheck. This column records when the combined validation
-- most recently completed — if it's newer than updated_at, we skip.

ALTER TABLE context_blocks
    ADD COLUMN dream_temporal_validated_at TIMESTAMPTZ;

-- Helps the Dream picker filter blocks that need validation vs already done.
CREATE INDEX IF NOT EXISTS idx_blocks_dream_temporal_pending
    ON context_blocks (id)
    WHERE dream_temporal_validated_at IS NULL
      AND NOT is_archived;
-- @@ ctx-fold end 028_block_temporal_validated.sql

-- @@ ctx-fold begin 029_block_is_meta.sql
-- Meta-Block-Exclude: flag blocks that produce systematic NO_REL noise in Dream.
-- Validated empirically on 2026-04-20 post-reset audit:
--   - 6/7 NO_REL-Links stammten aus Meta-Block-Beteiligung
--   - 0/28 CORRECT-Links würden durch den Filter verloren
--   - Accuracy-Delta: +12.4pp
--
-- Meta = generische Selbstbeschreibung, Methodik-Reflexion, oder Meta-Index
-- ohne spezifisches Thema. Breite semantische Nähe zu vielem, inhaltlich nicht verbunden.
--
-- Definition (OR):
--   1. category ∈ ('agent-briefing', 'index')
--   2. title startet mit 'GottZ CV '
--   3. title matcht '(Origin-Story|Motivations-Geschichte|Persönlichkeitsprofil|Agent Briefing|Compound-Loop-Modell)'
--   4. title = 'integration-upgrade'
--
-- Reviewbar via: SELECT id, title, category FROM context_blocks WHERE is_meta;

ALTER TABLE context_blocks
    ADD COLUMN is_meta BOOLEAN NOT NULL DEFAULT false;

-- Initial-Population nach validierter Definition
UPDATE context_blocks SET is_meta = true
WHERE NOT is_archived
  AND (
    category IN ('agent-briefing', 'index')
    OR title LIKE 'GottZ CV %'
    OR title ~* '(Origin-Story|Motivations-Geschichte|Persönlichkeitsprofil|Agent Briefing|Compound-Loop-Modell)'
    OR title = 'integration-upgrade'
  );

-- Picker-friendly index für Dream-Eligibility-Query
CREATE INDEX IF NOT EXISTS idx_blocks_is_meta
    ON context_blocks (id)
    WHERE is_meta;
-- @@ ctx-fold end 029_block_is_meta.sql

-- @@ ctx-fold begin 030_rrf_mass_factor.sql
-- =============================================================================
-- 030_rrf_mass_factor.sql — Mass-Factor IM RRF-Score (Branch X v3, Welle 34)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle 28 (X v1+v2): Mass als post-RRF Multiplikator (max 1.30 boost)
-- zu schwach gegen RRF-Score-Gap. Δ=-0.0139, beide Iterationen REJECTED.
--
-- v3 verlagert Mass IN den RRF-Score: jeder channel-rank wird mit
-- mass_factor multipliziert VOR der RRF-Aggregation. Mega-Blocks (high
-- num_dates) bekommen rank-Dämpfung VOR der Aggregation; single-date
-- Blocks behalten vollen rank (factor=1.0).
--
-- Mass-Definition:
--   mass_factor = 1 / sqrt(array_length(content_times, 1))   für blocks mit dates
--               = 1.0                                         für blocks ohne dates
--
-- Effekt:   1 date  → 1.000   (single-date keeps full RRF)
--           4 dates → 0.500
--          10 dates → 0.316
--         100 dates → 0.100   (Mega-Blocks gedämpft)
--
-- ApplyCyclicGravityBoost und ApplyGravityBoost (post-RRF) bleiben
-- unverändert — Mass und Cyclic-Phase-Boost sind orthogonale Effekte.
-- =============================================================================

-- Idempotent: drop the M020 9-param signature explicitly, then CREATE OR REPLACE.
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT);

CREATE OR REPLACE FUNCTION ctx_rrf(
    p_embedding           halfvec(1024),
    p_query               TEXT,
    p_query_spaced        TEXT,
    p_scopes              TEXT[],
    p_category            TEXT DEFAULT NULL,
    p_tags                TEXT[] DEFAULT NULL,
    p_limit               INT DEFAULT 5,
    p_temporal            TEXT DEFAULT NULL,
    p_query_or            TEXT DEFAULT NULL
) RETURNS TABLE (
    rrf_score   DOUBLE PRECISION,
    cosine_sim  DOUBLE PRECISION,
    id          UUID,
    title       VARCHAR(255),
    category    VARCHAR(100),
    tags        TEXT[],
    content     TEXT,
    scope       VARCHAR(20),
    updated_at  TIMESTAMPTZ
) LANGUAGE plpgsql AS $$
BEGIN
    SET LOCAL hnsw.iterative_scan = 'relaxed_order';

    RETURN QUERY
    WITH semantic AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
            ) AS rank,
            1.0 - (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS cos_sim
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
        ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
        LIMIT 75
    ),
    fulltext_de AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query)),
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_de @@ plainto_tsquery('german', p_query)
              OR cb.ts_de @@ plainto_tsquery('german', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    fulltext_en AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_en @@ plainto_tsquery('english', p_query)
              OR cb.ts_en @@ plainto_tsquery('english', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    trigram_title AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY similarity(cb.title, p_query) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND similarity(cb.title, p_query) > 0.05
        LIMIT 30
    ),
    -- v3: per-block mass_factor. Mega-Blocks (many content_times) get
    -- rank-damping; blocks without content_times keep the original rank.
    block_mass AS (
        SELECT cb.id,
               CASE
                   WHEN array_length(cb.content_times, 1) IS NULL THEN 1.0
                   WHEN array_length(cb.content_times, 1) = 0    THEN 1.0
                   ELSE 1.0 / sqrt(array_length(cb.content_times, 1)::DOUBLE PRECISION)
               END AS mass_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
    ),
    rrf AS (
        SELECT
            COALESCE(s.id, d.id, e.id, g.id) AS block_id,
            -- v3: mass_factor multipliziert auf jeden channel-rank VOR der
            -- RRF-Aggregation. NULL-safe via COALESCE(m.mass_factor, 1.0).
            (   0.45 * COALESCE(m.mass_factor, 1.0) * COALESCE(1.0 / (60 + s.rank), 0)
              + 0.20 * COALESCE(m.mass_factor, 1.0) * COALESCE(1.0 / (60 + d.rank), 0)
              + 0.25 * COALESCE(m.mass_factor, 1.0) * COALESCE(1.0 / (60 + e.rank), 0)
              + 0.10 * COALESCE(m.mass_factor, 1.0) * COALESCE(1.0 / (60 + g.rank), 0)
            )::DOUBLE PRECISION AS score,
            s.cos_sim::DOUBLE PRECISION
        FROM semantic s
        FULL OUTER JOIN fulltext_de d USING (id)
        FULL OUTER JOIN fulltext_en e USING (id)
        FULL OUTER JOIN trigram_title g USING (id)
        LEFT JOIN block_mass m ON m.id = COALESCE(s.id, d.id, e.id, g.id)
    )
    SELECT
        r.score         AS rrf_score,
        r.cos_sim       AS cosine_sim,
        cb.id,
        cb.title,
        cb.category,
        cb.tags,
        cb.content,
        cb.scope,
        cb.updated_at
    FROM rrf r
    JOIN context_blocks cb ON cb.id = r.block_id
    ORDER BY r.score DESC
    LIMIT p_limit;
END;
$$;
-- @@ ctx-fold end 030_rrf_mass_factor.sql

-- @@ ctx-fold begin 031_dream_links_recurrent.sql
-- =============================================================================
-- 031_dream_links_recurrent.sql — Recurrent relationship class (Welle 38b)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle 38b adds a fifth Dream-Link relationship: 'recurrent'.
--
-- Existing classes (topical/factual/causal/supersedes) cover semantic similarity,
-- knowledge-implication, decision-cause, and version-replacement. They miss the
-- recurring-instance case: blocks that share a temporal-pattern AND title-shape
-- but are valid in parallel (e.g. mautrix-{signal,whatsapp,discord} bridges,
-- Session-N handovers, sequential project-phase snapshots, periodised CV ranges).
--
-- Pre-Empirie 2026-05-06 (audit-w38b-results.json): 27 Phase-1 candidates
-- (sim>0.5, same dim+value in context_temporal) → 18 RECURRENT + 2 SUPERSEDES
-- + 7 NEITHER per Sub-Agent-Audit. 74% Phase-1 precision.
--
-- Detection in dream pipeline (recurrence.go):
--   Phase 1: SQL pre-filter (same dim+value + title-sim > 0.5)
--   Phase 2: LLM-confirm with explicit pattern classification
--            (weekly/monthly/sessional/parallel/sequence/version-replacement)
--
-- raw_confidence floor for 'recurrent' is 0.8 (linkfilters.go) — higher than
-- the 0.7 floor for other types because Phase 2 LLM-classification has more
-- error-modes than the existing topical/factual/causal evaluation.
-- =============================================================================

ALTER TABLE context_dream_links
  DROP CONSTRAINT IF EXISTS context_dream_links_relationship_check;

ALTER TABLE context_dream_links
  ADD CONSTRAINT context_dream_links_relationship_check
  CHECK (relationship = ANY (ARRAY[
    'topical'::text,
    'factual'::text,
    'causal'::text,
    'supersedes'::text,
    'recurrent'::text
  ]));

-- Partial index for recurrent-only queries (e.g. recurrent-cluster lookups,
-- supersedes-promotion-candidate scans).
CREATE INDEX IF NOT EXISTS idx_dream_links_recurrent
  ON context_dream_links (source_block_id, target_block_id)
  WHERE relationship = 'recurrent';

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (31, '031_dream_links_recurrent.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 031_dream_links_recurrent.sql

-- @@ ctx-fold begin 032_audit_blocks_is_meta.sql
-- =============================================================================
-- 032_audit_blocks_is_meta.sql — Korpus-Hygiene: ctx-system audit-blocks (Welle 38b)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Persistiert die ad-hoc is_meta=TRUE Markierung der 12 ctx-system-meta-blocks,
-- die während Welle 38b (2026-05-06) als top-5-noise für Cyclic-Queries
-- identifiziert wurden. Pattern bekannt aus S32 (MEMORY.md): welle-/session-/
-- audit-blocks dominieren als semantic-Distraktoren bei "donnerstags|mittwochs|
-- am wochenende"-Queries → C-Bucket Regression -16.7pp.
--
-- Kurzfrist-Mitigation (jetzt): is_meta=TRUE excludiert sie aus PickBlock
-- (dream pipeline schreibt keine neuen Links zu/von ihnen).
--
-- Strukturelle Lösung folgt in Welle 39: ctx_rrf is_meta-aware Filter (M033),
-- damit auch Retrieval die Drift-blocks ignoriert.
--
-- Idempotent: WHERE NOT is_meta — bei Re-Run kein Side-Effect.
--
-- Folge-Welle 40: ctx_save sollte automatic is_meta=TRUE für category=learnings
-- + tag~welle|session|audit setzen (kein reaktive Migration mehr nötig).
-- =============================================================================

UPDATE context_blocks SET is_meta = TRUE
WHERE id::text IN (
  '019dfeca-15f6-7534-b752-fd00b907e304',  -- Welle 38b HOLD-Audit (heute)
  '019dfe9e-5790-7196-ab10-adb47d03d22d',  -- Welle 38a NULL-RESULT (heute)
  '019dfa6b-cb82-7bb4-841a-34c9e008c161',  -- Welle 35/36/37 + v1.1.0
  '019df9e4-1070-746f-a0de-4801e164b324',  -- Welle 34/34a/34b
  '019df997-dbc9-7ab3-96ab-551f1ca67334',  -- Welle 33
  '019df938-2074-7a69-9950-181664744cc8',  -- Session 31 Welle 32
  '019df92c-eb19-7016-a175-2bf9a093e547',  -- Session 30 Welle 31
  '019df8ee-023d-74ee-8f64-ba39c5ff441c',  -- Session 28 Multi-Path-Bench
  '019df910-6647-7c5e-abfc-65fb9a9f29e9',  -- Session 29 Performance-Bench
  '019df858-e16e-7c7b-abb0-b119f3853a66',  -- Session 27 Behaviour-Layer
  '019defa4-c0f6-7c09-8e67-618f4d2af081',  -- Dream V3 Performance Audit
  '019df3e4-253a-7f0f-86e2-bc0eba4a6539',  -- Bench-Falle endogenes Sampling
  '019df23d-149a-7439-87f8-dcd3a0be47bb'   -- Bench-Setup qwen3.6:27b
)
AND NOT is_meta;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (32, '032_audit_blocks_is_meta.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 032_audit_blocks_is_meta.sql

-- @@ ctx-fold begin 033_rrf_is_meta_filter.sql
-- =============================================================================
-- 033_rrf_is_meta_filter.sql — ctx_rrf is_meta-aware Filter (Welle 39)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle 38b Bench-Investigation hat gezeigt: ctx-system-meta-blocks (welle/
-- session/audit) dominieren als top-5-noise für Cyclic-Queries (z.B.
-- "ddstatus donnerstags") → C-Bucket Regression -16.7pp obwohl Welle 38b code
-- write-side-only ist (recurrence.go schreibt nur context_dream_links). Pattern
-- bekannt aus S32 (MEMORY.md): ctx-system-blocks als semantic-Distraktoren.
--
-- Pre-Welle-Mitigation (M032): 13 audit-blocks als is_meta=TRUE markiert
-- (Korpus-Hygiene). Aber ctx_rrf hatte bisher KEIN is_meta-Filter — die
-- markierten Blocks erschienen weiter als Sources.
--
-- Diese Migration ergänzt `AND NOT cb.is_meta` in alle 4 RRF-Channels
-- (semantic, fulltext_de, fulltext_en, trigram_title) UND in der block_mass
-- CTE. Symmetrisch zu PickBlock (dream.go:319 `AND NOT is_meta`).
--
-- Effekt:
-- - 29 aktuelle is_meta-blocks (welle-audit, topic-maps, agent-briefings, CV,
--   origin-stories, integration-upgrade) verschwinden aus Retrieval-Sources
-- - eval.sh testet keine is_meta-relevant queries (CV/topic-map/briefing
--   sind nicht in 47-case-set)
-- - eval-cyclic-gold rationales beschreiben topic-map/origin-stories explizit
--   als zu-filternde Mega-Block-Noise — diese Migration implementiert genau das
--
-- Risiko: real-world queries die explizit CV/profile-info brauchen würden
-- nichts mehr finden. Mitigation für Folge-Welle: opt-in `--include-meta` flag
-- im query-API (out-of-scope für Welle 39).
--
-- Symmetrisch + idempotent: Function CREATE OR REPLACE.
-- =============================================================================

DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT);

CREATE OR REPLACE FUNCTION ctx_rrf(
    p_embedding           halfvec(1024),
    p_query               TEXT,
    p_query_spaced        TEXT,
    p_scopes              TEXT[],
    p_category            TEXT DEFAULT NULL,
    p_tags                TEXT[] DEFAULT NULL,
    p_limit               INT DEFAULT 5,
    p_temporal            TEXT DEFAULT NULL,
    p_query_or            TEXT DEFAULT NULL
) RETURNS TABLE (
    rrf_score   DOUBLE PRECISION,
    cosine_sim  DOUBLE PRECISION,
    id          UUID,
    title       VARCHAR(255),
    category    VARCHAR(100),
    tags        TEXT[],
    content     TEXT,
    scope       VARCHAR(20),
    updated_at  TIMESTAMPTZ
) LANGUAGE plpgsql AS $$
BEGIN
    SET LOCAL hnsw.iterative_scan = 'relaxed_order';

    RETURN QUERY
    WITH semantic AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
            ) AS rank,
            1.0 - (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS cos_sim
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND NOT cb.is_meta
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
        ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
        LIMIT 75
    ),
    fulltext_de AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query)),
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND NOT cb.is_meta
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_de @@ plainto_tsquery('german', p_query)
              OR cb.ts_de @@ plainto_tsquery('german', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    fulltext_en AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND NOT cb.is_meta
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_en @@ plainto_tsquery('english', p_query)
              OR cb.ts_en @@ plainto_tsquery('english', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    trigram_title AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY similarity(cb.title, p_query) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND NOT cb.is_meta
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND similarity(cb.title, p_query) > 0.05
        LIMIT 30
    ),
    -- v3: per-block mass_factor. Mega-Blocks (many content_times) get
    -- rank-damping; blocks without content_times keep the original rank.
    -- Welle 39 (M033): is_meta-Filter symmetrisch zu allen 4 channels.
    block_mass AS (
        SELECT cb.id,
               CASE
                   WHEN array_length(cb.content_times, 1) IS NULL THEN 1.0
                   WHEN array_length(cb.content_times, 1) = 0    THEN 1.0
                   ELSE 1.0 / sqrt(array_length(cb.content_times, 1)::DOUBLE PRECISION)
               END AS mass_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND NOT cb.is_meta
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
    ),
    rrf AS (
        SELECT
            COALESCE(s.id, d.id, e.id, g.id) AS block_id,
            (   0.45 * COALESCE(m.mass_factor, 1.0) * COALESCE(1.0 / (60 + s.rank), 0)
              + 0.20 * COALESCE(m.mass_factor, 1.0) * COALESCE(1.0 / (60 + d.rank), 0)
              + 0.25 * COALESCE(m.mass_factor, 1.0) * COALESCE(1.0 / (60 + e.rank), 0)
              + 0.10 * COALESCE(m.mass_factor, 1.0) * COALESCE(1.0 / (60 + g.rank), 0)
            )::DOUBLE PRECISION AS score,
            s.cos_sim::DOUBLE PRECISION
        FROM semantic s
        FULL OUTER JOIN fulltext_de d USING (id)
        FULL OUTER JOIN fulltext_en e USING (id)
        FULL OUTER JOIN trigram_title g USING (id)
        LEFT JOIN block_mass m ON m.id = COALESCE(s.id, d.id, e.id, g.id)
    )
    SELECT
        r.score         AS rrf_score,
        r.cos_sim       AS cosine_sim,
        cb.id,
        cb.title,
        cb.category,
        cb.tags,
        cb.content,
        cb.scope,
        cb.updated_at
    FROM rrf r
    JOIN context_blocks cb ON cb.id = r.block_id
    ORDER BY r.score DESC
    LIMIT p_limit;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (33, '033_rrf_is_meta_filter.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 033_rrf_is_meta_filter.sql

-- @@ ctx-fold begin 034_audit_blocks_unmeta.sql
-- =============================================================================
-- 034_audit_blocks_unmeta.sql — is_meta=FALSE für legitime retrieval-targets
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle 39 Bench-Verdict (M033 ctx_rrf is_meta-Filter): C-Bucket gefixt
-- 56% → 89% (+6 cases), aber M-Bucket -6.7pp + L-Bucket -12pp regression.
--
-- Root-Cause: M032 hatte mehrere blocks als is_meta=TRUE markiert die in
-- eval-cyclic-gold als EXPECTED retrieval-targets definiert sind:
-- - C-003 (ctx motivation am wochenende)         → 019d7c3b (Motivations)
-- - C-017 (session 24 dream deployment...)       → 019dc04b (Session 24 Dream)
-- - M-003 (dream v3 performance letzte woche)    → 019defa4 (Dream V3 Audit)
-- - M-009 (bench setup qwen letzte woche)        → 019df23d (Bench-Setup qwen)
-- - M-010 (session 27 testcontainer vor woche)   → 019df858 (Session 27)
-- - L-002 (session 27 testcontainer behaviour)   → 019df858 (Session 27)
-- - L-015 (bench falle endogenes sampling)       → 019df3e4 (Bench-Falle)
-- - L-018 (gottz cv berufserfahrung 2021)        → 019c4a34-835d (CV 2017-2021)
-- - L-023 (ddstatus mediawiki migration februar) → 019defb1 (ddstatus mediawiki)
--
-- Erkenntnis: Welle/Session/Audit/CV-blocks sind LEGITIME Wissen-blocks die
-- retrievable sein müssen. is_meta=TRUE (= retrieval-exclude via M033) war
-- für sie falsch. M033 bleibt korrekt für ECHT-meta-blocks (topic-maps,
-- agent-briefings, integration-upgrade, ctx Origin, Compound-Loop).
--
-- Dieses Migration revertet is_meta=FALSE für die 8 fälschlich als meta
-- markierten expected-blocks. Plus zusätzlich für andere wave-/session-/
-- audit-blocks die ähnlich legitim sein könnten als specific-query-targets:
--
-- Idempotent (nur explizite IDs). Folge-Welle (40+) muss bessere Klassifikation
-- finden (z.B. multi-level is_meta oder score-damping statt hard-exclude).
-- =============================================================================

UPDATE context_blocks SET is_meta = FALSE
WHERE id::text IN (
  -- Aus eval-cyclic-gold direkt expected:
  '019d7c3b-7f87-766a-95e0-65313d89fb7a',  -- ctx Motivations-Geschichte (C-003)
  '019dc04b-9024-7fdd-8ed7-e6465b5d54dc',  -- Session 24 Dream v3 Deployment (C-017)
  '019defa4-c0f6-7c09-8e67-618f4d2af081',  -- Dream V3 Performance Audit (M-003)
  '019df23d-149a-7439-87f8-dcd3a0be47bb',  -- Bench-Setup qwen3.6:27b (M-009)
  '019df858-e16e-7c7b-abb0-b119f3853a66',  -- Session 27 testcontainer (M-010, L-002)
  '019df3e4-253a-7f0f-86e2-bc0eba4a6539',  -- Bench-Falle endogenes Sampling (L-015)
  '019c4a34-835d-743a-ae53-5b89e260295d',  -- CV Berufserfahrung 2017-2021 (L-018)
  '019defb1-8817-7c84-be8e-5096727e6cd0',  -- ddstatus mediawiki migration (L-023)
  -- CV-Familie (semantisch konsistent zu L-018):
  '019c4a34-7eb3-7b12-856a-859905c5c721',  -- CV Berufserfahrung 2022-2026
  '019c4a35-b786-7bac-9a6e-1685d73d12a6',  -- CV Berufserfahrung 2008-2016
  '019c4a34-043a-7b8f-afed-c13edb5febf7',  -- CV Profil und Kontaktdaten
  '019c4a36-230d-7f77-9853-2aa0728bc351',  -- CV Skills und Zertifizierungen
  '019c4a35-f408-7c1b-871f-9ec4b3df1373'   -- CV Projekte und Ausbildung
)
AND is_meta;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (34, '034_audit_blocks_unmeta.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 034_audit_blocks_unmeta.sql

-- @@ ctx-fold begin 035_block_role_classification.sql
-- =============================================================================
-- 035_block_role_classification.sql — Block-Role 4-Klassen-Enum (Welle 40)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle 39 (M033) ctx_rrf is_meta-aware Filter ist binär: hard-exclude oder
-- full-pass. M034 musste 13 fälschlich als is_meta=TRUE markierte legitime
-- retrieval-targets reverten — der binäre Mechanismus konnte audit-trail von
-- knowledge nicht differenzieren.
--
-- Welle 40 Architektur C (Hybrid) löst das durch 4-Klassen-Enum block_role:
--
--   system-meta  — topic-maps, agent-briefings, integration-upgrade, ctx
--                  Origin/Compound-Loop. Hard-exclude in ctx_rrf (analog
--                  is_meta=TRUE). Für Retrieval irrelevant.
--   audit-trail  — Session-Handover, Welle-Audit, Bench-Snapshots. Score-
--                  Damping *0.3 in ctx_rrf — sichtbar bei explicit-target
--                  queries, gedämpft als noise gegen knowledge-blocks.
--   reference    — CV, Profile, Kontaktdaten. Full-pass (gleich wie knowledge).
--                  Eigene Klasse für künftige Filter (z.B. category-only-Mode).
--   knowledge    — Default. Alle Decisions, Erkenntnisse, Projekt-Wissen,
--                  Architektur, Bugs. Full-pass.
--
-- Decision-Quelle: .project/bench-dream-phase0/BRANCH-HYPOTHESIS-Welle-40-
-- Klassifikation.md (Architektur C Hybrid).
--
-- Backfill-Strategie (deterministisch, idempotent):
--   1. is_meta=TRUE → block_role='system-meta' (hard-exclude bleibt, M033-
--      Semantik wird via M036 auf block_role!='system-meta' umgestellt)
--   2. category='reference' UND NOT is_meta → block_role='reference'
--   3. Hardcoded ID-Liste der 12 Sub-Agent-B-klassifizierten audit-trail-
--      blocks → block_role='audit-trail' (nur wenn NOT is_meta; system-meta
--      hat Vorrang, keine Doppelklassifikation)
--   4. Rest bleibt auf DEFAULT 'knowledge'
--
-- Index-Strategie: partial Index nur auf nicht-knowledge blocks (~50 von 449
-- aktiven). Sparse-aware, klein genug um in shared_buffers zu bleiben.
--
-- Idempotent: ALTER ADD COLUMN ... DEFAULT setzt Werte nur initial. CHECK
-- constraint additiv. UPDATE-Backfill mit AND-Bedingungen erlaubt Re-Run
-- ohne Effekt.
-- =============================================================================

ALTER TABLE context_blocks
  ADD COLUMN IF NOT EXISTS block_role TEXT NOT NULL DEFAULT 'knowledge';

ALTER TABLE context_blocks
  DROP CONSTRAINT IF EXISTS context_blocks_block_role_check;

ALTER TABLE context_blocks
  ADD CONSTRAINT context_blocks_block_role_check
  CHECK (block_role IN ('system-meta', 'audit-trail', 'reference', 'knowledge'));

CREATE INDEX IF NOT EXISTS idx_context_blocks_block_role
  ON context_blocks(block_role)
  WHERE block_role != 'knowledge';

-- Backfill 1: system-meta aus is_meta=TRUE (≈ 19 blocks).
-- M033/M036 Semantik bleibt hard-exclude erhalten.
UPDATE context_blocks
   SET block_role = 'system-meta'
 WHERE is_meta = TRUE
   AND block_role = 'knowledge';

-- Backfill 2: reference aus category='reference' (≈ 121 blocks).
-- Nur wenn NOT is_meta (system-meta hat Vorrang).
UPDATE context_blocks
   SET block_role = 'reference'
 WHERE NOT is_meta
   AND category = 'reference'
   AND block_role = 'knowledge';

-- Backfill 3: audit-trail (12 IDs aus Sub-Agent-B Klassifikation).
-- Nur wenn NOT is_meta — system-meta hat Vorrang. UUIDs sind Session-
-- handovers, Welle-Audits, Bench-Setups die als specific-target queries
-- erreichbar bleiben sollen, aber als noise gegen knowledge-blocks gedämpft.
UPDATE context_blocks
   SET block_role = 'audit-trail'
 WHERE id::text IN (
   '019df858-e16e-7c7b-abb0-b119f3853a66', -- Session 27 testcontainer (M034-revert audit-trail)
   '019dc04b-9024-7fdd-8ed7-e6465b5d54dc', -- Session 24 Dream v3 (M034-revert audit-trail)
   '019defa4-c0f6-7c09-8e67-618f4d2af081', -- Dream V3 Performance Audit 2026-05-03 (M034-revert)
   '019d74a6-f1ec-7894-93eb-438bafc5d303', -- ctx Security Audit 2026-04-10
   '019d7430-5319-7d7b-ae61-be11e7967666', -- Claude Chat MCP Feedback Session 23
   '019d73f3-9d31-7593-a13d-0a124ad066e4', -- Session 23 Handover
   '019d6f2c-cfe3-7a2a-8ea1-03727c5fdec0', -- Session 22 T02-T13 Audit-Ergebnis
   '019d4d68-8c28-7cf9-b922-b9783768eec4', -- Session 18 Workflow Improvements
   '019d41c4-d2ff-7023-9087-947f2e1272ab', -- Dream Mode Phase 1 Implementation Session 15
   '019d40fc-c07e-7ce6-b19c-ba0bf6b9f0d1', -- Session 14 CI/CD Fix
   '019d3dd4-0b2c-7014-9c9d-7af073086744', -- Session 11 Warning-Versagen
   '019d3bea-eea1-7c13-a671-41971bdd3392'  -- Session 11 Go CLI + Ingestion Pipeline
 )
   AND NOT is_meta
   AND block_role = 'knowledge';

-- Validation (post-apply, comment-only — nicht executable):
-- SELECT block_role, COUNT(*) FROM context_blocks
--  WHERE NOT is_archived GROUP BY block_role ORDER BY COUNT(*) DESC;
-- Erwartung: knowledge >> reference (~121) > system-meta (~19) > audit-trail (~12)

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (35, '035_block_role_classification.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 035_block_role_classification.sql

-- @@ ctx-fold begin 036_rrf_block_role_filter.sql
-- =============================================================================
-- 036_rrf_block_role_filter.sql — ctx_rrf block_role-aware Filter+Damping (W40)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle 39 (M033) hat is_meta-Filter binär (`AND NOT cb.is_meta`) in alle 4
-- RRF-Channels und block_mass eingebaut. M034 musste reverten weil binär nicht
-- differenziert: legitime audit-trail-blocks (Session-Handover, Bench-Audits)
-- mussten retrievable bleiben, aber als noise dominierten sie eval-cyclic top-5.
--
-- Welle 40 ersetzt is_meta-Filter durch block_role-aware Mechanismus
-- (siehe M035 für Klassen-Definition):
--
--   system-meta  → hard-exclude (ersetzt `AND NOT cb.is_meta`)
--   audit-trail  → Score-Damping *0.3 (sichtbar bei explicit-target queries,
--                  gedämpft als noise gegen knowledge-blocks)
--   reference    → full-pass (gleich wie knowledge)
--   knowledge    → full-pass (default)
--
-- Damping-Faktor 0.3: empirisch motiviert aus Welle-39 Bench-Investigation.
-- audit-trail-blocks dominierten top-5 in C-Bucket eval-cyclic Cases mit
-- Faktor ~3-5 score-überschuss gegenüber knowledge-targets. *0.3 dämpft
-- audit-trail unter knowledge wenn beide vergleichbare RRF-Ranks haben,
-- erhält aber Sichtbarkeit wenn audit-trail der einzige Treffer ist
-- (z.B. "session 27 testcontainer" als specific-target query).
--
-- Implementierung: neuer CTE `block_role_factor` analog zu `block_mass`. JOIN
-- in der rrf-CTE, Multiplikation per channel-rank zusätzlich zu mass_factor.
-- COALESCE(rf.role_factor, 1.0) als defensive default für edge-cases.
--
-- Symmetrisch + idempotent: Function CREATE OR REPLACE.
-- =============================================================================

DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT);

CREATE OR REPLACE FUNCTION ctx_rrf(
    p_embedding           halfvec(1024),
    p_query               TEXT,
    p_query_spaced        TEXT,
    p_scopes              TEXT[],
    p_category            TEXT DEFAULT NULL,
    p_tags                TEXT[] DEFAULT NULL,
    p_limit               INT DEFAULT 5,
    p_temporal            TEXT DEFAULT NULL,
    p_query_or            TEXT DEFAULT NULL
) RETURNS TABLE (
    rrf_score   DOUBLE PRECISION,
    cosine_sim  DOUBLE PRECISION,
    id          UUID,
    title       VARCHAR(255),
    category    VARCHAR(100),
    tags        TEXT[],
    content     TEXT,
    scope       VARCHAR(20),
    updated_at  TIMESTAMPTZ
) LANGUAGE plpgsql AS $$
BEGIN
    SET LOCAL hnsw.iterative_scan = 'relaxed_order';

    RETURN QUERY
    WITH semantic AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
            ) AS rank,
            1.0 - (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS cos_sim
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
        ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
        LIMIT 75
    ),
    fulltext_de AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query)),
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_de @@ plainto_tsquery('german', p_query)
              OR cb.ts_de @@ plainto_tsquery('german', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    fulltext_en AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_en @@ plainto_tsquery('english', p_query)
              OR cb.ts_en @@ plainto_tsquery('english', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    trigram_title AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY similarity(cb.title, p_query) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND similarity(cb.title, p_query) > 0.05
        LIMIT 30
    ),
    -- v3: per-block mass_factor. Mega-Blocks (many content_times) get
    -- rank-damping; blocks without content_times keep the original rank.
    -- Welle 40 (M036): system-meta hard-excluded via block_role-Filter.
    block_mass AS (
        SELECT cb.id,
               CASE
                   WHEN array_length(cb.content_times, 1) IS NULL THEN 1.0
                   WHEN array_length(cb.content_times, 1) = 0    THEN 1.0
                   ELSE 1.0 / sqrt(array_length(cb.content_times, 1)::DOUBLE PRECISION)
               END AS mass_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
    ),
    -- Welle 40 (M036): per-block role_factor. audit-trail wird *0.3 gedämpft,
    -- knowledge/reference behalten Faktor 1.0. system-meta erscheint hier nicht
    -- (bereits durch hard-exclude in den 4 channels gefiltert), aber der CTE
    -- excluded sie defensiv mit gleichem Filter — damit COALESCE den 1.0-default
    -- nur für edge-cases (NULL block_role) trifft.
    block_role_factor AS (
        SELECT cb.id,
               CASE
                   WHEN cb.block_role = 'audit-trail' THEN 0.3
                   ELSE 1.0
               END AS role_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
    ),
    rrf AS (
        SELECT
            COALESCE(s.id, d.id, e.id, g.id) AS block_id,
            -- v3: mass_factor multipliziert auf jeden channel-rank VOR der
            -- RRF-Aggregation. Welle 40 (M036): zusätzlich role_factor — beide
            -- multiplikativ, damit audit-trail-mega-blocks doppelt gedämpft
            -- werden. NULL-safe via COALESCE(*, 1.0).
            (   0.45 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + s.rank), 0)
              + 0.20 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + d.rank), 0)
              + 0.25 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + e.rank), 0)
              + 0.10 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + g.rank), 0)
            )::DOUBLE PRECISION AS score,
            s.cos_sim::DOUBLE PRECISION
        FROM semantic s
        FULL OUTER JOIN fulltext_de d USING (id)
        FULL OUTER JOIN fulltext_en e USING (id)
        FULL OUTER JOIN trigram_title g USING (id)
        LEFT JOIN block_mass m ON m.id = COALESCE(s.id, d.id, e.id, g.id)
        LEFT JOIN block_role_factor rf ON rf.id = COALESCE(s.id, d.id, e.id, g.id)
    )
    SELECT
        r.score         AS rrf_score,
        r.cos_sim       AS cosine_sim,
        cb.id,
        cb.title,
        cb.category,
        cb.tags,
        cb.content,
        cb.scope,
        cb.updated_at
    FROM rrf r
    JOIN context_blocks cb ON cb.id = r.block_id
    ORDER BY r.score DESC
    LIMIT p_limit;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (36, '036_rrf_block_role_filter.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 036_rrf_block_role_filter.sql

-- @@ ctx-fold begin 037_rrf_role_damping_tuning.sql
-- =============================================================================
-- 037_rrf_role_damping_tuning.sql — audit-trail Damping 0.3 → 0.5 (Welle 40 it.4)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- M036 (Welle 40 it.2/3) hat Damping-Faktor 0.3 für audit-trail eingeführt.
-- Iteration-3-Bench-Verdict: eval-cyclic mean_pass 0.8728 vs Re-Baseline 0.9428
-- = -7pp REGRESSION durch 5 NEG flips, alle bei explicit-audit-trail-target
-- queries:
--   L-002 "session 27 testcontainer behaviour"     → 019df858 audit-trail
--   L-009 "session 22 audit ergebnis"              → 019d6f2c audit-trail
--   M-003 "dream v3 performance letzte woche"      → 019defa4 audit-trail
--   M-004 "session 24 dream vor 2 wochen"          → 019dc04b audit-trail
--   M-015 "ctx security audit anfang april 2026"   → 019d74a6 audit-trail
--
-- Bei 0.3-damping fallen audit-trail-blocks auch bei explicit-target queries
-- aus top-5. eval.sh T03 ("recent embedding changes") wurde gefixt aber zu
-- hoher Preis (5 explicit-queries verloren).
--
-- Iteration 4 (Welle 40 Hypothese): Damping 0.3 → 0.5. Damit:
--   - explicit-target queries finden audit-trail-blocks weiterhin (RRF-rank
--     dieser blocks ist bei explicit-query deutlich höher als knowledge-
--     Konkurrenz, *0.5 reicht um top-5 zu halten)
--   - generic-recent queries: knowledge-blocks haben oft vergleichbare RRF-
--     rank zu audit-trail. *0.5 dämpft audit-trail unter knowledge wenn
--     ranks ≈ gleich. T03 sollte bestehen.
--
-- Symmetrisch + idempotent: Function CREATE OR REPLACE.
-- =============================================================================

DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT);

CREATE OR REPLACE FUNCTION ctx_rrf(
    p_embedding           halfvec(1024),
    p_query               TEXT,
    p_query_spaced        TEXT,
    p_scopes              TEXT[],
    p_category            TEXT DEFAULT NULL,
    p_tags                TEXT[] DEFAULT NULL,
    p_limit               INT DEFAULT 5,
    p_temporal            TEXT DEFAULT NULL,
    p_query_or            TEXT DEFAULT NULL
) RETURNS TABLE (
    rrf_score   DOUBLE PRECISION,
    cosine_sim  DOUBLE PRECISION,
    id          UUID,
    title       VARCHAR(255),
    category    VARCHAR(100),
    tags        TEXT[],
    content     TEXT,
    scope       VARCHAR(20),
    updated_at  TIMESTAMPTZ
) LANGUAGE plpgsql AS $$
BEGIN
    SET LOCAL hnsw.iterative_scan = 'relaxed_order';

    RETURN QUERY
    WITH semantic AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
            ) AS rank,
            1.0 - (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS cos_sim
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
        ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
        LIMIT 75
    ),
    fulltext_de AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query)),
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_de @@ plainto_tsquery('german', p_query)
              OR cb.ts_de @@ plainto_tsquery('german', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    fulltext_en AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_en @@ plainto_tsquery('english', p_query)
              OR cb.ts_en @@ plainto_tsquery('english', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    trigram_title AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY similarity(cb.title, p_query) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND similarity(cb.title, p_query) > 0.05
        LIMIT 30
    ),
    block_mass AS (
        SELECT cb.id,
               CASE
                   WHEN array_length(cb.content_times, 1) IS NULL THEN 1.0
                   WHEN array_length(cb.content_times, 1) = 0    THEN 1.0
                   ELSE 1.0 / sqrt(array_length(cb.content_times, 1)::DOUBLE PRECISION)
               END AS mass_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
    ),
    -- Welle 40 (M037): per-block role_factor. audit-trail wird *0.5 gedämpft
    -- (M036 hatte 0.3, war zu aggressiv — fünf explicit-target queries
    -- verloren ihre audit-trail-targets aus top-5). 0.5 erlaubt explicit-
    -- target Findung weiter und dämpft trotzdem in generic-recent queries.
    block_role_factor AS (
        SELECT cb.id,
               CASE
                   WHEN cb.block_role = 'audit-trail' THEN 0.5
                   ELSE 1.0
               END AS role_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
    ),
    rrf AS (
        SELECT
            COALESCE(s.id, d.id, e.id, g.id) AS block_id,
            (   0.45 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + s.rank), 0)
              + 0.20 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + d.rank), 0)
              + 0.25 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + e.rank), 0)
              + 0.10 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + g.rank), 0)
            )::DOUBLE PRECISION AS score,
            s.cos_sim::DOUBLE PRECISION
        FROM semantic s
        FULL OUTER JOIN fulltext_de d USING (id)
        FULL OUTER JOIN fulltext_en e USING (id)
        FULL OUTER JOIN trigram_title g USING (id)
        LEFT JOIN block_mass m ON m.id = COALESCE(s.id, d.id, e.id, g.id)
        LEFT JOIN block_role_factor rf ON rf.id = COALESCE(s.id, d.id, e.id, g.id)
    )
    SELECT
        r.score         AS rrf_score,
        r.cos_sim       AS cosine_sim,
        cb.id,
        cb.title,
        cb.category,
        cb.tags,
        cb.content,
        cb.scope,
        cb.updated_at
    FROM rrf r
    JOIN context_blocks cb ON cb.id = r.block_id
    ORDER BY r.score DESC
    LIMIT p_limit;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (37, '037_rrf_role_damping_tuning.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 037_rrf_role_damping_tuning.sql

-- @@ ctx-fold begin 038_rrf_role_damping_revert.sql
-- =============================================================================
-- 038_rrf_role_damping_revert.sql — audit-trail Damping → 1.0 (Welle 40 HOLD)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle 40 Iteration-3 (M036 damping=0.3) und Iteration-4 (M037 damping=0.5)
-- Bench-Verdict empirisch identisch: mean_pass 0.8728 (vs Re-Baseline 0.9428
-- = -7pp REGRESSION). 0/70 cases mit unterschiedlichen top5 zwischen 0.3 und
-- 0.5 — Damping-Faktor-Tuning ist nicht der wirksame Hebel.
--
-- Stop-Bedingung der Welle 40 Spec (2 Iterationen ohne improvement → HOLD)
-- erreicht. Welle 40 wird NICHT als v1.3.0 promoted.
--
-- Diese Migration stellt den Production-State zurück auf v1.2.0-Niveau:
-- damping_factor 1.0 für audit-trail = effektiv kein damping. M035 Schema
-- (block_role enum + Backfill) bleibt deployed für Folge-Welle 41+
-- (query-aware damping design).
--
-- Funktional ist M038 = M033 (Welle 39 ctx_rrf is_meta-aware) modulo:
-- - Filter via block_role != 'system-meta' (statt NOT is_meta) — semantisch
--   identisch nach M035-Backfill (is_meta=TRUE ↔ block_role='system-meta')
-- - block_role_factor CTE bleibt (forwards-kompatibel für Folge-Welle)
--
-- Symmetrisch + idempotent: Function CREATE OR REPLACE.
-- =============================================================================

DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT);

CREATE OR REPLACE FUNCTION ctx_rrf(
    p_embedding           halfvec(1024),
    p_query               TEXT,
    p_query_spaced        TEXT,
    p_scopes              TEXT[],
    p_category            TEXT DEFAULT NULL,
    p_tags                TEXT[] DEFAULT NULL,
    p_limit               INT DEFAULT 5,
    p_temporal            TEXT DEFAULT NULL,
    p_query_or            TEXT DEFAULT NULL
) RETURNS TABLE (
    rrf_score   DOUBLE PRECISION,
    cosine_sim  DOUBLE PRECISION,
    id          UUID,
    title       VARCHAR(255),
    category    VARCHAR(100),
    tags        TEXT[],
    content     TEXT,
    scope       VARCHAR(20),
    updated_at  TIMESTAMPTZ
) LANGUAGE plpgsql AS $$
BEGIN
    SET LOCAL hnsw.iterative_scan = 'relaxed_order';

    RETURN QUERY
    WITH semantic AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
            ) AS rank,
            1.0 - (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS cos_sim
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
        ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
        LIMIT 75
    ),
    fulltext_de AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query)),
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_de @@ plainto_tsquery('german', p_query)
              OR cb.ts_de @@ plainto_tsquery('german', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    fulltext_en AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_en @@ plainto_tsquery('english', p_query)
              OR cb.ts_en @@ plainto_tsquery('english', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    trigram_title AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY similarity(cb.title, p_query) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND similarity(cb.title, p_query) > 0.05
        LIMIT 30
    ),
    block_mass AS (
        SELECT cb.id,
               CASE
                   WHEN array_length(cb.content_times, 1) IS NULL THEN 1.0
                   WHEN array_length(cb.content_times, 1) = 0    THEN 1.0
                   ELSE 1.0 / sqrt(array_length(cb.content_times, 1)::DOUBLE PRECISION)
               END AS mass_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
    ),
    -- Welle 40 HOLD (M038): role_factor=1.0 für ALLE block_role (incl. audit-
    -- trail). Effektiv kein damping. Iteration 3 (0.3) und Iteration 4 (0.5)
    -- haben identische -7pp regression durch knowledge-overshoot bei explicit-
    -- audit-trail-target queries. Damping-Faktor-Tuning ist nicht der wirk-
    -- same Hebel. Stop-Bedingung 2-Iter-ohne-improvement erreicht.
    -- CTE bleibt für forwards-Compatibility; Folge-Welle 41 implementiert
    -- query-aware damping (z.B. Tag-detection in query).
    block_role_factor AS (
        SELECT cb.id,
               1.0::DOUBLE PRECISION AS role_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
    ),
    rrf AS (
        SELECT
            COALESCE(s.id, d.id, e.id, g.id) AS block_id,
            (   0.45 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + s.rank), 0)
              + 0.20 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + d.rank), 0)
              + 0.25 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + e.rank), 0)
              + 0.10 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + g.rank), 0)
            )::DOUBLE PRECISION AS score,
            s.cos_sim::DOUBLE PRECISION
        FROM semantic s
        FULL OUTER JOIN fulltext_de d USING (id)
        FULL OUTER JOIN fulltext_en e USING (id)
        FULL OUTER JOIN trigram_title g USING (id)
        LEFT JOIN block_mass m ON m.id = COALESCE(s.id, d.id, e.id, g.id)
        LEFT JOIN block_role_factor rf ON rf.id = COALESCE(s.id, d.id, e.id, g.id)
    )
    SELECT
        r.score         AS rrf_score,
        r.cos_sim       AS cosine_sim,
        cb.id,
        cb.title,
        cb.category,
        cb.tags,
        cb.content,
        cb.scope,
        cb.updated_at
    FROM rrf r
    JOIN context_blocks cb ON cb.id = r.block_id
    ORDER BY r.score DESC
    LIMIT p_limit;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (38, '038_rrf_role_damping_revert.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 038_rrf_role_damping_revert.sql

-- @@ ctx-fold begin 039_rrf_query_aware_damping.sql
-- =============================================================================
-- 039_rrf_query_aware_damping.sql — Query-aware audit-trail damping (Welle 41)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle 40 HOLD-Lehre: uniform damping (M036=0.3, M037=0.5) ist nicht der
-- Hebel — bei den 5 NEG flips von Welle 40 ist audit-trail-rank 1 und
-- knowledge-rank 2-5; selbst 0.5x dämpft audit-trail unter knowledge.
-- Pre-Empirie Welle 41 Iter 0: bei 70 eval-cyclic-cases haben 7 audit-trail-
-- target queries, 6 davon mit Pattern "session"/"welle"/"audit"/"recurrent"/
-- "handover"/"self-audit" detected (Recall 0.86, Precision 0.75).
--
-- Welle 41 Architektur: Query-aware damping. Damping-Faktor wird vom Caller
-- (Go-side) abhängig von Pattern-Detection im Query-String passiert. SQL-
-- Function akzeptiert neuen Parameter p_audit_trail_factor mit DEFAULT 1.0.
--
-- Regel:
--   - Query enthält Pattern → Caller passt 1.0 (no damping, audit-trail
--     erscheint normal in top-5)
--   - Query enthält KEIN Pattern → Caller passt 0.3 (audit-trail unter
--     knowledge gedämpft, generic-recent queries finden knowledge first)
--
-- Schema-Default 1.0: backward-kompatibel zu M038. Dream-cycle und andere
-- nicht-pattern-aware caller behalten no-damping-Verhalten ohne Code-Change.
--
-- Welle-40 Lessons applied:
--   - W12 1-Commit-pro-Änderung: SQL-signature first (no-op default), Go-
--     Caller-Updates separate Commits.
--   - W3 Pre-Empirie: 0.86 recall ist messbar (welle-41-pattern-audit.json).
--   - Edge case M-003 ("dream v3 performance letzte woche") ist FN ohne
--     Pattern — bleibt fail bis Pattern-List erweitert oder M-003 expected
--     re-classified (Welle 42+).
-- =============================================================================

DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT);
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT, DOUBLE PRECISION);

CREATE OR REPLACE FUNCTION ctx_rrf(
    p_embedding             halfvec(1024),
    p_query                 TEXT,
    p_query_spaced          TEXT,
    p_scopes                TEXT[],
    p_category              TEXT DEFAULT NULL,
    p_tags                  TEXT[] DEFAULT NULL,
    p_limit                 INT DEFAULT 5,
    p_temporal              TEXT DEFAULT NULL,
    p_query_or              TEXT DEFAULT NULL,
    p_audit_trail_factor    DOUBLE PRECISION DEFAULT 1.0
) RETURNS TABLE (
    rrf_score   DOUBLE PRECISION,
    cosine_sim  DOUBLE PRECISION,
    id          UUID,
    title       VARCHAR(255),
    category    VARCHAR(100),
    tags        TEXT[],
    content     TEXT,
    scope       VARCHAR(20),
    updated_at  TIMESTAMPTZ
) LANGUAGE plpgsql AS $$
BEGIN
    SET LOCAL hnsw.iterative_scan = 'relaxed_order';

    RETURN QUERY
    WITH semantic AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
            ) AS rank,
            1.0 - (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS cos_sim
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
        ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
        LIMIT 75
    ),
    fulltext_de AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query)),
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_de @@ plainto_tsquery('german', p_query)
              OR cb.ts_de @@ plainto_tsquery('german', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    fulltext_en AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (
              cb.ts_en @@ plainto_tsquery('english', p_query)
              OR cb.ts_en @@ plainto_tsquery('english', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    trigram_title AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY similarity(cb.title, p_query) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND similarity(cb.title, p_query) > 0.05
        LIMIT 30
    ),
    block_mass AS (
        SELECT cb.id,
               CASE
                   WHEN array_length(cb.content_times, 1) IS NULL THEN 1.0
                   WHEN array_length(cb.content_times, 1) = 0    THEN 1.0
                   ELSE 1.0 / sqrt(array_length(cb.content_times, 1)::DOUBLE PRECISION)
               END AS mass_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
    ),
    -- Welle 41 (M039): query-aware audit-trail damping. Caller-controlled via
    -- p_audit_trail_factor parameter. DEFAULT 1.0 = no damping (backward-
    -- kompatibel zu M038, dream-cycle und Tests). Pattern-Detection in
    -- internal/rrf/pattern.go (Go-side) setzt 0.3 für non-audit-target queries.
    block_role_factor AS (
        SELECT cb.id,
               CASE
                   WHEN cb.block_role = 'audit-trail' THEN p_audit_trail_factor
                   ELSE 1.0
               END AS role_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
    ),
    rrf AS (
        SELECT
            COALESCE(s.id, d.id, e.id, g.id) AS block_id,
            (   0.45 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + s.rank), 0)
              + 0.20 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + d.rank), 0)
              + 0.25 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + e.rank), 0)
              + 0.10 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + g.rank), 0)
            )::DOUBLE PRECISION AS score,
            s.cos_sim::DOUBLE PRECISION
        FROM semantic s
        FULL OUTER JOIN fulltext_de d USING (id)
        FULL OUTER JOIN fulltext_en e USING (id)
        FULL OUTER JOIN trigram_title g USING (id)
        LEFT JOIN block_mass m ON m.id = COALESCE(s.id, d.id, e.id, g.id)
        LEFT JOIN block_role_factor rf ON rf.id = COALESCE(s.id, d.id, e.id, g.id)
    )
    SELECT
        r.score         AS rrf_score,
        r.cos_sim       AS cosine_sim,
        cb.id,
        cb.title,
        cb.category,
        cb.tags,
        cb.content,
        cb.scope,
        cb.updated_at
    FROM rrf r
    JOIN context_blocks cb ON cb.id = r.block_id
    ORDER BY r.score DESC
    LIMIT p_limit;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (39, '039_rrf_query_aware_damping.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 039_rrf_query_aware_damping.sql

-- @@ ctx-fold begin 040_audit_trail_classification_extension.sql
-- =============================================================================
-- 040_audit_trail_classification_extension.sql — Audit-Trail-Klassifikation
-- erweitert (Welle 43)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle 40 hatte 12 audit-trail-Blocks via Sub-Agent-B-Klassifikation (n=20
-- candidates, 11 audit-trail + 9 knowledge identifiziert). Sub-Agent-A SQL-
-- Audit hatte aber 47 audit-trail-Kandidaten gefunden — 35 blieben unaudited.
--
-- Welle 43 schließt die Lücke: Sub-Agent-Audit n=34 candidates (Stand
-- 2026-05-07: 297 knowledge mit audit/welle/session-Heuristik-Match).
-- Klassifikations-Verteilung: 9 audit-trail / 25 knowledge / 2 whitelisted.
--
-- Whitelist (block_role bleibt 'knowledge', explizit geschützt):
--   - 019d33fd-...: "Session 7 — Go Migration gestartet" (M-013/C-006 expected)
--   - 019defb1-...: "ddstatus mediawiki migration audit" (L-023 expected)
-- Begründung: diese blocks sind in eval-cyclic-gold expected, ihre queries
-- haben KEIN audit-pattern — als audit-trail klassifiziert würden sie via
-- Welle-41-query-aware-damping (*0.3 ohne pattern) aus top-5 fallen.
--
-- Cross-Check (welle-43-classification.json): 9 audit-trail-IDs sind 0× in
-- eval-cyclic-gold expected — keine Bench-Regression-Risk durch UPDATE.
--
-- Pattern-Bestätigung der 9 IDs (alle Session-Handover/Audit-Trail-Format):
--   1. Session 4 Projekt-Status-Notiz (019d2bfc)
--   2. Warning #6/#8 RLHF-Bias Process-Audit (019d3980)
--   3. Session 2 Summary mit 59 Sub-Agent-Phasen (019d28e7)
--   4. Session 2 Meta-Erkenntnis Process-Audit (019d29af)
--   5. Session 7 Abschlussbilanz (019d3445)
--   6. Session 7 eval.sh Gate PASSED Bench-Snapshot (019d3444)
--   7. Session 3 Review Selbstkritik gegen warnings.md (019d2afa)
--   8. Session 9 TODO-Backlog priorisiert (019d398d)
--   9. LLM Temporal Normalization Test Results Session 9 (019d394b)
--
-- Idempotent (nur explizite IDs, AND block_role = 'knowledge' guard).
-- =============================================================================

UPDATE context_blocks SET block_role = 'audit-trail'
WHERE id::text IN (
  '019d2bfc-86ca-7310-ac16-650739b6e329',  -- Session 4 — Projekt ctx etabliert
  '019d3980-772b-7b4d-b9bf-6eae952277cd',  -- Warning #6/#8 — Persistenter RLHF-Bias (Session 7+9 Pattern)
  '019d28e7-3990-75a9-9786-bd0f45faa20d',  -- Session 2 Summary (2026-03-26)
  '019d29af-a201-7b5f-9aba-1da631d65c2b',  -- Session 2 Meta-Erkenntnis: Context-Engineering > Feature-Engineering
  '019d3445-6226-7320-affa-ab11c0bdf7c7',  -- Session 7 — Abschlussbilanz
  '019d3444-6c84-7463-98b2-1aec0b7ff2a3',  -- Session 7 — eval.sh Gate PASSED
  '019d2afa-7af0-7089-926b-42f49cdec845',  -- Session 3 Review — Selbstkritik gegen warnings.md
  '019d398d-6a03-76b4-84ef-6bc1060449ee',  -- Session 9 TODO-Backlog — Priorisiert nach Impact
  '019d394b-bc3f-7ac8-9e69-9039aafba31f'   -- LLM Temporal Normalization — Test Results Session 9
)
AND block_role = 'knowledge';

-- Validation (post-apply, comment-only):
-- SELECT block_role, COUNT(*) FROM context_blocks WHERE NOT is_archived
--   GROUP BY block_role ORDER BY COUNT(*) DESC;
-- Erwartung: knowledge ~288 (-9), audit-trail ~22 (+9), reference 121, system-meta 22

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (40, '040_audit_trail_classification_extension.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 040_audit_trail_classification_extension.sql

-- @@ ctx-fold begin 041_dream_links_cleanup_dangling.sql
-- =============================================================================
-- 041_dream_links_cleanup_dangling.sql — Dangling-Link-Cleanup (Welle 45)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle 45 Topologie-Audit (Sub-Agent 3, 2026-05-22) hat 2 dangling dream_links
-- aufgedeckt, beide Seiten archiviert seit 2026-05-12:
--   - 019e1b5f (waf-test-02-shell-keyword) → 019e1b60 (waf-test-08-delete-and-update)
--   - 019e1b5f (waf-test-02-shell-keyword) → 019e1b60 (waf-test-09-drop-and-update)
--
-- Ursache: dream.CleanupDanglingLinks (go/internal/dream/dream.go:489) existiert
-- als toter Code — wird von keinem Caller invoked. Welle 45 schließt die Lücke
-- a) hier (einmalig DELETE) und b) im Code (Aufruf in runDailySynthesis, plus
-- Erweiterung auf source_block_id archived).
--
-- Idempotent: NOT EXISTS-Guard prüft auf is_archived. Re-Anwendung löscht
-- nichts wenn alle bereits aufgeräumt sind.
-- =============================================================================

DELETE FROM context_dream_links
WHERE source_block_id IN (SELECT id FROM context_blocks WHERE is_archived)
   OR target_block_id IN (SELECT id FROM context_blocks WHERE is_archived);

-- Validation (post-apply, comment-only):
-- SELECT COUNT(*) FROM context_dream_links dl
--   JOIN context_blocks src ON src.id = dl.source_block_id
--   JOIN context_blocks tgt ON tgt.id = dl.target_block_id
--   WHERE src.is_archived OR tgt.is_archived;
-- Erwartung: 0

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (41, '041_dream_links_cleanup_dangling.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 041_dream_links_cleanup_dangling.sql

-- @@ ctx-fold begin 042_dream_full_reset.sql
-- =====================================================================
-- 042_dream_full_reset.sql — Full Dream Reset (Welle 46)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =====================================================================
-- Welle 46 Konvention-Switch (Sub-Agent 2 Semantic-Audit 2026-05-22):
-- supersedes-Direction-Convention auf englische Sprachsemantik
-- ("A supersedes B" → A is newer/authoritative). Statt einzelnen
-- Records zu swappen wird der gesamte dream-Korpus reset und über
-- alle 510 Blocks neu evaluiert. Saubere Baseline, alle neuen Links
-- per neuer Konvention (enforceSupersedesDirection SWAP-Filter).
--
-- Plus: löst implicit auch
--   - 8 phantom-pending blocks (M041 Vorgänger-Pattern)
--   - 92 Pre-v5 Links (kein dream_version-Mix mehr nach Re-Dream)
--   - 10 isolated blocks (kompletter Re-Run gibt allen erneut Chance)
--   - 4 archived-dangling links (TRUNCATE löscht eh alles)
--
-- Idempotent: TRUNCATE auf leerer Tabelle = no-op,
--             UPDATE auf bereits NULL-Werten = no-op.
-- =====================================================================

TRUNCATE TABLE context_dream_links;

UPDATE context_blocks
   SET dream_checked_at = NULL,
       dream_cooldown_until = NULL,
       dream_keywords = NULL,
       dream_temporal_validated_at = NULL
 WHERE NOT is_archived
   AND embedding IS NOT NULL
   AND (block_type IS NULL OR block_type IN ('knowledge', 'source', 'canonical'))
   AND NOT is_meta;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (42, '042_dream_full_reset.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 042_dream_full_reset.sql

-- @@ ctx-fold begin 043_supersedes_direction_swap.sql
-- =============================================================================
-- 043_supersedes_direction_swap.sql — Supersedes Direction Swap (Welle 46)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle 46 Dream-Quality-Audit (Sub-Agent 2 Semantic-Stichprobe, 2026-05-22):
--   5/5 supersedes-Stichproben sind INVERTED — TGT.created_at > SRC.created_at,
--   obwohl die natuerliche Sprachsemantik "A supersedes B" → A.created_at >=
--   B.created_at verlangt (A = die neuere, ersetzende Version). Bei 15
--   supersedes-Links insgesamt ergab die Vollerhebung: ALLE 15 invertiert.
--
-- Ursache: causal-Klasse hat die Inversion (S23-Pathologie) im Code-Filter
-- mit acceptCausal(srcCreated.Before(tgtCreated)) repariert. supersedes hat
-- die invertierte Filter-Konvention geerbt (acceptSupersedes verlangt
-- src.updated_at < tgt.updated_at). Welle 46 setzt die Sprach-Semantik
-- (source = neuer) als Korpus-Wahrheit durch.
--
-- Reciprocal-Konflikte: 4 von 15 Pairs haben in der Gegenrichtung einen
-- topical-Link. Nach Swap wuerde die PK (source, target) kollidieren.
-- Resolution: supersedes ist semantisch spezifischer als topical →
-- topical-reverse wird DELETEd, supersedes als swapped INSERT eingesetzt.
--   - 019c48f3 ↔ 019d314c (sup 0.95 vs topical 0.70)
--   - 019d28e0-9170 ↔ 019d28e1-1702 (sup 0.90 vs topical 0.90)
--   - 019db565 ↔ 019e16f6 (sup 0.90 vs topical 0.85)
--   - 019d2a50 ↔ 019d33de-9cf9 (sup 0.85 vs topical 0.85)
--
-- Strategy: DELETE invertierte + ggf. reverse-topical, INSERT mit getauschten
-- IDs. UPDATE des PK waere ebenfalls moeglich, DELETE+INSERT ist sauberer
-- (kein Trigger-Side-Effect, expliziter audit-trail in created_at = now()).
--
-- Idempotent: re-apply prueft auf src.created_at < tgt.created_at und matched
-- nur noch nicht geswappte Links. Erwartung bei Re-Apply: 0 Rows affected.
--
-- Bench-Risk: SupersedesMap (dream/dream.go:528) und filterSuperseded
-- (handler/query.go:577) interpretieren supersedes nach alter Konvention
-- (source = old). Nach diesem Swap haben sie invertierte Semantik —
-- DOKUMENTIERT, Welle 47+ Folge-Arbeit. Bench-Risk gering, weil die 15
-- betroffenen Source-IDs nicht in .eval-cyclic-gold.json sind.
-- =============================================================================


-- Step 1: snapshot the invertierten supersedes-Links in temp-Table.
-- Wir lesen die ungeswappten Rohdaten BEVOR DELETE laeuft.
CREATE TEMP TABLE supersedes_to_swap ON COMMIT DROP AS
SELECT
  dl.source_block_id,
  dl.target_block_id,
  dl.relationship,
  dl.confidence,
  dl.raw_confidence,
  dl.scope,
  dl.metadata,
  dl.dream_version
FROM context_dream_links dl
JOIN context_blocks src ON src.id = dl.source_block_id
JOIN context_blocks tgt ON tgt.id = dl.target_block_id
WHERE dl.relationship = 'supersedes'
  AND src.created_at < tgt.created_at;

-- Step 2: clear reverse-topical-Konflikte. Nach Swap ist (target, source)
-- der neue PK; wenn dort bereits ein Link existiert (irgendeine relationship),
-- muss er weg. In der Praxis sind das die 4 reciprocal topical-Pairs.
DELETE FROM context_dream_links dl
WHERE EXISTS (
  SELECT 1 FROM supersedes_to_swap s
  WHERE s.source_block_id = dl.target_block_id
    AND s.target_block_id = dl.source_block_id
);

-- Step 3: delete die invertierten supersedes-Links.
DELETE FROM context_dream_links dl
WHERE EXISTS (
  SELECT 1 FROM supersedes_to_swap s
  WHERE s.source_block_id = dl.source_block_id
    AND s.target_block_id = dl.target_block_id
);

-- Step 4: reinsert mit getauschten source/target. metadata-Erweiterung
-- dokumentiert den Swap fuer spaetere Audits.
INSERT INTO context_dream_links (
  source_block_id, target_block_id, relationship,
  confidence, raw_confidence, scope, metadata, dream_version, created_at
)
SELECT
  target_block_id AS source_block_id,
  source_block_id AS target_block_id,
  relationship,
  confidence,
  raw_confidence,
  scope,
  COALESCE(metadata, '{}'::jsonb) || '{"direction_swap_w46": true, "audit_source": "sub-agent-2-2026-05-22"}'::jsonb AS metadata,
  dream_version,
  now() AS created_at
FROM supersedes_to_swap
ON CONFLICT (source_block_id, target_block_id) DO NOTHING;


-- Validation (post-apply, comment-only):
-- SELECT count(*) FROM context_dream_links dl
--   JOIN context_blocks src ON src.id = dl.source_block_id
--   JOIN context_blocks tgt ON tgt.id = dl.target_block_id
--   WHERE dl.relationship = 'supersedes'
--     AND src.created_at < tgt.created_at;
-- Erwartung: 0 (kein invertierter Link mehr).
--
-- SELECT count(*) FROM context_dream_links WHERE relationship = 'supersedes';
-- Erwartung: 15 (oder 11 bei strikter Reciprocal-Resolution; abhaengig von
-- ON CONFLICT-Hits — siehe metadata.direction_swap_w46 fuer Tracking).

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (43, '043_supersedes_direction_swap.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 043_supersedes_direction_swap.sql

-- @@ ctx-fold begin 044_title_text.sql
-- =============================================================================
-- 044_title_text.sql — context_blocks.title VARCHAR(255) → TEXT (v2.0.0)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Trigger: CRAG-Bench S38c uncovered Wikipedia-Titel + Chunk-Suffix
-- Truncation (255-char limit hit for synthetic chunked content). v2.0.0
-- removes the length-Constraint to allow long titles for ingested
-- web/wiki corpora.
--
-- Mechanism: title is referenced by two STORED generated columns
-- (ts_de, ts_en) defined in 001_initial.sql. PostgreSQL refuses
-- ALTER COLUMN ... TYPE on a column used by a generated column, so we
-- drop the dependent columns, alter title, then recreate them
-- (which repopulates the tsvectors from current data) plus their GIN
-- indexes — all inside one transaction so failure leaves the schema
-- intact.
--
-- Cost: ts_de / ts_en are STORED → recreation re-tokenizes every row.
-- This is the only non-metadata-only step in M044-M047. At small/medium
-- corpus sizes (≤100k blocks) it completes in seconds; at 1M+ blocks
-- plan for tens of seconds of write-amplification + index rebuild.
--
-- The two GIN indexes idx_context_ts_de / idx_context_ts_en disappear
-- with the columns and are recreated identically. Other indexes on
-- context_blocks are untouched.
--
-- The uq_context_category_title constraint on (category, title) is
-- unaffected — UNIQUE constraints don't pin a column type beyond
-- equality semantics, which TEXT and VARCHAR share.
--
-- Idempotent via _migrations PK + ON CONFLICT.
-- Reversible: ALTER COLUMN title TYPE VARCHAR(255) (with the same
-- drop-recreate dance for ts_de/ts_en) — fails if any row exceeds
-- 255 chars after roll-forward (pre-rollback filter needed).
-- =============================================================================


ALTER TABLE context_blocks DROP COLUMN ts_de;
ALTER TABLE context_blocks DROP COLUMN ts_en;

ALTER TABLE context_blocks ALTER COLUMN title TYPE TEXT;

ALTER TABLE context_blocks
    ADD COLUMN ts_de TSVECTOR GENERATED ALWAYS AS (
        to_tsvector('german', coalesce(title::text, '') || ' ' || content)
    ) STORED;

ALTER TABLE context_blocks
    ADD COLUMN ts_en TSVECTOR GENERATED ALWAYS AS (
        to_tsvector('english', coalesce(title::text, '') || ' ' || content)
    ) STORED;

CREATE INDEX IF NOT EXISTS idx_context_ts_de ON context_blocks USING GIN(ts_de);
CREATE INDEX IF NOT EXISTS idx_context_ts_en ON context_blocks USING GIN(ts_en);

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (44, '044_title_text.sql', now())
  ON CONFLICT (version) DO NOTHING;

-- @@ ctx-fold end 044_title_text.sql

-- @@ ctx-fold begin 045_drop_chk_scope.sql
-- =============================================================================
-- 045_drop_chk_scope.sql — DROP chk_scope constraint (v2.0.0)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Trigger: CRAG-Bench B5 finding — the hard-coded scope-Liste
-- (private|work|shared) prevents adding new scopes (e.g. 'crag') without
-- a schema migration. v2.0.0 makes scope-validation a runtime concern
-- (API-Key allowed_scopes + home_scope already gate access).
--
-- Properties: ALTER TABLE ... DROP CONSTRAINT is metadata-only (no row
-- re-validation). idx_context_scope and the underlying column remain.
--
-- Breaking-Change-Note (per v2-roadmap.md): clients that relied on
-- "rejected at INSERT" for invalid scope strings must validate
-- themselves. Existing rows with private/work/shared remain valid.
--
-- Idempotent: DROP CONSTRAINT IF EXISTS — safe on re-apply.
-- Reversible: ADD CONSTRAINT chk_scope CHECK (scope IN (...)) — fails
-- if any row contains a scope outside the original whitelist.
--
-- Note: chk_blob_scope on context_blobs is NOT touched here (different
-- migration if/when blobs gain multi-tenant scope variants).
-- =============================================================================

ALTER TABLE context_blocks DROP CONSTRAINT IF EXISTS chk_scope;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (45, '045_drop_chk_scope.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 045_drop_chk_scope.sql

-- @@ ctx-fold begin 046_write_log_block_title_text.sql
-- =============================================================================
-- 046_write_log_block_title_text.sql — context_write_log.block_title TEXT (v2.0.0)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Trigger: M044 made context_blocks.title TEXT. context_write_log mirrors
-- title via block_title at write-time for audit-trail purposes. With M044
-- VARCHAR(500) becomes inconsistent — a long title would silently truncate
-- in the audit log. v2.0.0 makes both sides TEXT for full fidelity.
--
-- Properties: metadata-only ALTER (no row re-write).
--
-- Idempotent via _migrations PK + ON CONFLICT.
-- Reversible: ALTER COLUMN block_title TYPE VARCHAR(500) — fails if any
-- row exceeds 500 chars after roll-forward.
-- =============================================================================

ALTER TABLE context_write_log ALTER COLUMN block_title TYPE TEXT;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (46, '046_write_log_block_title_text.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 046_write_log_block_title_text.sql

-- @@ ctx-fold begin 047_scope_varchar50.sql
-- =============================================================================
-- 047_scope_varchar50.sql — context_blocks.scope VARCHAR(20)→VARCHAR(50) (v2.0.0)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Trigger: M045 dropped chk_scope, enabling arbitrary scope strings for
-- multi-tenant deployments. VARCHAR(20) is too narrow for compound or
-- prefixed scope names (e.g. 'tenant:crag-research-q1' or
-- 'org-1234567:archive'). Bumping to VARCHAR(50) keeps a sane upper bound
-- without ALTER-ing to TEXT.
--
-- Why VARCHAR(50) not TEXT: scope is indexed (idx_context_scope) and used
-- as ANY-Array element in queries (W47-11 backlog). A bounded length
-- preserves planner statistics quality and prevents accidental abuse
-- (e.g. someone storing entire paragraphs in scope).
--
-- Properties: ALTER COLUMN ... TYPE VARCHAR(50) is metadata-only when
-- widening (no row re-validation). All existing scope ≤20-char values
-- remain valid.
--
-- Idempotent via _migrations PK + ON CONFLICT.
-- Reversible: ALTER COLUMN scope TYPE VARCHAR(20) — fails if any row
-- has scope > 20 chars after roll-forward.
-- =============================================================================

ALTER TABLE context_blocks ALTER COLUMN scope TYPE VARCHAR(50);

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (47, '047_scope_varchar50.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 047_scope_varchar50.sql

-- @@ ctx-fold begin 048_rrf_exclude_params.sql
-- =============================================================================
-- 048_rrf_exclude_params.sql — ctx_rrf categories_exclude + block_roles_exclude
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle 47 / v2.0.0 C2: API-side exclude parameters für /api/query.
--
-- Trigger: CRAG-Bench n=10 movie (Session 38c) zeigte topic-map-private Block
-- (category='index', block_role='audit-trail'/'synthesis') als Slot-Stealer
-- in 4/10 Variant-A Top-5. In n=10 kein Score-Impact, aber strukturelles
-- Risk-Faktor bei größerem Korpus.
--
-- Lösung: zwei optionale Parameter ans Ende der ctx_rrf-Signatur:
--   - p_categories_exclude TEXT[]   — Blöcke mit category IN(...) ausschließen
--   - p_block_roles_exclude TEXT[]  — Blöcke mit block_role IN(...) ausschließen
--
-- Schema-Defaults NULL = no-op exclude (backward-kompatibel). Existing caller
-- (dream-cycle, Tests) brauchen keine Code-Änderung.
--
-- WHERE-Klausel-Pattern (siehe semantic-CTE, identisch in fulltext_de/en/
-- trigram_title/block_mass/block_role_factor):
--   AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
--   AND (p_block_roles_exclude IS NULL OR cb.block_role IS NULL
--        OR cb.block_role != ALL(p_block_roles_exclude))
--
-- block_role IS NULL-Branch verhindert NULL != ALL(...)-Pitfall: in PG ist
-- NULL != ALL(ARRAY[...]) immer NULL (nicht TRUE), filtert sonst Legacy-
-- Blocks ohne expliziten block_role aus. Explicit OR cb.block_role IS NULL
-- erhält sie.
-- =============================================================================

DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT, DOUBLE PRECISION);
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT, DOUBLE PRECISION, TEXT[], TEXT[]);

CREATE OR REPLACE FUNCTION ctx_rrf(
    p_embedding             halfvec(1024),
    p_query                 TEXT,
    p_query_spaced          TEXT,
    p_scopes                TEXT[],
    p_category              TEXT DEFAULT NULL,
    p_tags                  TEXT[] DEFAULT NULL,
    p_limit                 INT DEFAULT 5,
    p_temporal              TEXT DEFAULT NULL,
    p_query_or              TEXT DEFAULT NULL,
    p_audit_trail_factor    DOUBLE PRECISION DEFAULT 1.0,
    p_categories_exclude    TEXT[] DEFAULT NULL,
    p_block_roles_exclude   TEXT[] DEFAULT NULL
) RETURNS TABLE (
    rrf_score   DOUBLE PRECISION,
    cosine_sim  DOUBLE PRECISION,
    id          UUID,
    title       TEXT,
    category    VARCHAR(100),
    tags        TEXT[],
    content     TEXT,
    scope       VARCHAR(50),
    updated_at  TIMESTAMPTZ
) LANGUAGE plpgsql AS $$
BEGIN
    SET LOCAL hnsw.iterative_scan = 'relaxed_order';

    RETURN QUERY
    WITH semantic AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
            ) AS rank,
            1.0 - (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS cos_sim
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.block_role IS NULL OR cb.block_role != ALL(p_block_roles_exclude))
        ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
        LIMIT 75
    ),
    fulltext_de AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query)),
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.block_role IS NULL OR cb.block_role != ALL(p_block_roles_exclude))
          AND (
              cb.ts_de @@ plainto_tsquery('german', p_query)
              OR cb.ts_de @@ plainto_tsquery('german', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    fulltext_en AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.block_role IS NULL OR cb.block_role != ALL(p_block_roles_exclude))
          AND (
              cb.ts_en @@ plainto_tsquery('english', p_query)
              OR cb.ts_en @@ plainto_tsquery('english', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    trigram_title AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY similarity(cb.title, p_query) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.block_role IS NULL OR cb.block_role != ALL(p_block_roles_exclude))
          AND similarity(cb.title, p_query) > 0.05
        LIMIT 30
    ),
    block_mass AS (
        SELECT cb.id,
               CASE
                   WHEN array_length(cb.content_times, 1) IS NULL THEN 1.0
                   WHEN array_length(cb.content_times, 1) = 0    THEN 1.0
                   ELSE 1.0 / sqrt(array_length(cb.content_times, 1)::DOUBLE PRECISION)
               END AS mass_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.block_role IS NULL OR cb.block_role != ALL(p_block_roles_exclude))
    ),
    block_role_factor AS (
        SELECT cb.id,
               CASE
                   WHEN cb.block_role = 'audit-trail' THEN p_audit_trail_factor
                   ELSE 1.0
               END AS role_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.block_role IS NULL OR cb.block_role != ALL(p_block_roles_exclude))
    ),
    rrf AS (
        SELECT
            COALESCE(s.id, d.id, e.id, g.id) AS block_id,
            (   0.45 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + s.rank), 0)
              + 0.20 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + d.rank), 0)
              + 0.25 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + e.rank), 0)
              + 0.10 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + g.rank), 0)
            )::DOUBLE PRECISION AS score,
            s.cos_sim::DOUBLE PRECISION
        FROM semantic s
        FULL OUTER JOIN fulltext_de d USING (id)
        FULL OUTER JOIN fulltext_en e USING (id)
        FULL OUTER JOIN trigram_title g USING (id)
        LEFT JOIN block_mass m ON m.id = COALESCE(s.id, d.id, e.id, g.id)
        LEFT JOIN block_role_factor rf ON rf.id = COALESCE(s.id, d.id, e.id, g.id)
    )
    SELECT
        r.score                    AS rrf_score,
        r.cos_sim                  AS cosine_sim,
        cb.id,
        cb.title::TEXT             AS title,
        cb.category,
        cb.tags,
        cb.content,
        cb.scope::VARCHAR(50)      AS scope,
        cb.updated_at
    FROM rrf r
    JOIN context_blocks cb ON cb.id = r.block_id
    ORDER BY r.score DESC
    LIMIT p_limit;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (48, '048_rrf_exclude_params.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 048_rrf_exclude_params.sql

-- @@ ctx-fold begin 049_dream_backoff.sql
-- 049_dream_backoff.sql
-- Wave-2 (W49c): gradual back-off cooldown for the Dream re-dream loop.
--
-- The effective re-dream interval grows with the number of completed eval cycles
-- per block (dream_eval_count). Mature/legacy blocks (dreamed many times) are
-- pulled progressively less often, while NEW blocks (count 0) keep catching up
-- at the base rate. Curve shape (log/exp/linear), factor, grace, and cap are
-- runtime-configurable; see SetDreamCooldown. Transient GPU/LLM failures use the
-- minutes-cooldown path and do NOT increment the count, so the back-off reflects
-- real completed work only.
--
-- dream_eval_count is back-filled from the existing dream-eval LLM log so legacy
-- blocks enter the long-cooldown regime immediately on deploy. The deploy
-- timestamp is the "history split": pre/post back-off behavior is cleanly
-- separated in context_llm_log for the 1-month STEP-0 re-measurement.

ALTER TABLE context_blocks
    ADD COLUMN IF NOT EXISTS dream_eval_count INTEGER NOT NULL DEFAULT 0;

-- Back-fill from completed dream-eval cycles. The source block is block_ids[1]
-- (EvaluateRelationships logs source first, then candidates). Guarded so a fresh
-- install without context_llm_log still applies cleanly (column stays at 0).
DO $$
BEGIN
    IF to_regclass('public.context_llm_log') IS NOT NULL THEN
        UPDATE context_blocks b
        SET dream_eval_count = sub.cnt
        FROM (
            SELECT block_ids[1] AS sid, count(*)::int AS cnt
            FROM context_llm_log
            WHERE pipeline = 'dream-eval'
              AND error IS NULL
              AND block_ids IS NOT NULL
              AND array_length(block_ids, 1) >= 1
            GROUP BY block_ids[1]
        ) sub
        WHERE b.id = sub.sid;
    END IF;
END $$;
-- @@ ctx-fold end 049_dream_backoff.sql

-- @@ ctx-fold begin 050_dream_links_graph_traversal.sql
-- =============================================================================
-- 050_dream_links_graph_traversal.sql — covering indexes for query-time
-- Dream-graph traversal (GottZ Graph Expansion, Wave 1)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Wave 1: a new post-RRF Go stage (rrf.GraphExpand) 1-hop-expands the top RRF
-- seeds along the four POSITIVE Dream link types (topical/factual/causal/
-- recurrent) and fuses the neighbors via a Go boost. The negative 'supersedes'
-- type stays out — it is consumed by filterSuperseded as a drop-filter, never
-- as a traversal edge.
--
-- The traversal does ONE batched edge-fetch keyed by seed id, in BOTH directions
-- (inbound: target_block_id = ANY(seeds); outbound: source_block_id = ANY(seeds))
-- with a per-edge gate on raw_confidence and an ORDER BY raw_confidence DESC for
-- the per-seed cap (ROW_NUMBER()). No covering index existed for that access
-- pattern: idx_dream_links_target (M016) covers only the target column, and the
-- forward direction had only the primary key (source_block_id, target_block_id).
--
-- Two composite PARTIAL COVERING indexes — one per direction — make the gate +
-- per-seed-cap a pure index scan: the leading column is the seed side, the
-- second column gives the raw_confidence DESC ordering for free, and the
-- INCLUDE payload (the opposite block id + relationship + weighted confidence)
-- lets the planner satisfy the window/cap without a heap fetch on the link row.
-- The partial predicate (relationship <> 'supersedes') keeps the indexes lean —
-- they only carry the four traversed types.
--
-- Additive + idempotent: CREATE INDEX IF NOT EXISTS. The graph stage itself is
-- gated default-OFF in Go (GraphConfig.Enabled), so this migration is pure
-- infrastructure — it changes no query behavior on its own.
-- =============================================================================

-- Outbound traversal: source_block_id = ANY(seeds), ordered by raw_confidence.
CREATE INDEX IF NOT EXISTS idx_dream_links_graph_fwd
    ON context_dream_links (source_block_id, raw_confidence DESC)
    INCLUDE (target_block_id, relationship, confidence)
    WHERE relationship <> 'supersedes';

-- Inbound traversal: target_block_id = ANY(seeds), ordered by raw_confidence.
CREATE INDEX IF NOT EXISTS idx_dream_links_graph_rev
    ON context_dream_links (target_block_id, raw_confidence DESC)
    INCLUDE (source_block_id, relationship, confidence)
    WHERE relationship <> 'supersedes';

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (50, '050_dream_links_graph_traversal.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 050_dream_links_graph_traversal.sql

-- @@ ctx-fold begin 051_settings_store.sql
-- =============================================================================
-- 051_settings_store.sql — runtime settings overrides, audit trail, sealed
-- provider secrets (F2: settings persistence)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- context_settings: DB override layer on top of env/defaults. Precedence at
-- runtime: request body (only keys flagged request_overridable, never
-- sensitive/structural) > context_settings > env > code default. One row per
-- (key, scope); deleting the row reverts to env/default. scope is provisioned
-- now ('_global' sentinel; the underscore prefix is SYSTEM-RESERVED and
-- enforced in Go at api-key-create since 052) so the unique constraint never
-- needs a swap-migration once per-tenant settings arrive (target scale:
-- multi-tenant). F2 code reads scope='_global' exclusively.
--
-- context_settings_audit: append-only history, written by AFTER-ROW TRIGGERS
-- on context_settings AND context_secrets — covers API writes, psql direct
-- edits and break-glass factory reset alike, atomically with the mutation
-- (no Go-side audit INSERT, no crash window between write and audit).
-- entity_type/action carry NO CHECK constraints: values are derived from
-- TG_TABLE_NAME/TG_OP inside the trigger (closed set by construction), and
-- hard value lists in the schema are the migration class M045 abolished
-- (v2.0.0 line: validation is a runtime concern).
-- api_key_id has deliberately NO FK: audit rows reference, they never
-- cascade — a key delete must not anonymize history (ON DELETE SET NULL
-- would). actor_label snapshots the key label at write time for the same
-- reason. Settings values in old/new are never sensitive thanks to the
-- secret_ref reject in the settings API (422, F2-W5); secret rows carry
-- NULL values by construction (see audit_settings_write below).
--
-- context_secrets: AES-256-GCM sealed provider credentials, encrypted in Go
-- (crypto/aes stdlib), master key from env CTX_SECRETS_KEY — NOT pgcrypto:
-- pgp_sym_encrypt would ship the master key through the SQL wire protocol
-- into pg_stat_statements/log_statement paths. AAD binds name+scope, so a
-- ciphertext copied onto another row fails authentication.
-- =============================================================================

CREATE TABLE IF NOT EXISTS context_settings (
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    key         TEXT NOT NULL,
    -- TODO(multi-tenant): per-tenant settings resolve on this column
    -- (tenant scope overrides on top of '_global'); F2 reads '_global' only.
    scope       VARCHAR(50) NOT NULL DEFAULT '_global',
    value       JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by  UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,
    metadata    JSONB NOT NULL DEFAULT '{}',
    CONSTRAINT uq_settings_key_scope UNIQUE (key, scope)
);

CREATE TABLE IF NOT EXISTS context_settings_audit (
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    entity_type TEXT NOT NULL,                -- 'setting' | 'secret' (trigger: TG_TABLE_NAME; no CHECK, v2.0.0 line)
    entity_key  TEXT NOT NULL,                -- settings key resp. secret name
    scope       VARCHAR(50) NOT NULL DEFAULT '_global',
    action      TEXT NOT NULL,                -- set|unset|create|rotate|delete (trigger: TG_OP; no CHECK)
    old_value   JSONB,                        -- ALWAYS NULL for entity_type='secret'
    new_value   JSONB,                        -- ALWAYS NULL for entity_type='secret'
    api_key_id  UUID,                         -- deliberately NO FK (must never cascade, see header)
    actor_label TEXT,                         -- label snapshot at write time
    metadata    JSONB NOT NULL DEFAULT '{}',  -- via: api|sql, request_id (when set)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_settings_audit_key
    ON context_settings_audit (entity_key, created_at DESC);

CREATE TABLE IF NOT EXISTS context_secrets (
    id           UUID PRIMARY KEY DEFAULT uuidv7(),
    name         TEXT NOT NULL,               -- format validated in Go (no CHECK, v2.0.0 line)
    -- TODO(multi-tenant): per-tenant secrets resolve on this column as well.
    scope        VARCHAR(50) NOT NULL DEFAULT '_global',
    ciphertext   BYTEA NOT NULL,              -- GCM output incl. auth tag
    nonce        BYTEA NOT NULL,              -- 12 bytes, fresh per encryption
    key_version  INT NOT NULL DEFAULT 1,      -- master-key generation (diagnostics/rotation sweep)
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by   UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,
    rotated_at   TIMESTAMPTZ,
    rotated_by   UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,
    metadata     JSONB NOT NULL DEFAULT '{}',
    CONSTRAINT uq_secrets_name_scope UNIQUE (name, scope)
);

-- Hot-reload channel: covers API writes (redundant, idempotent) AND
-- break-glass/psql SQL edits — on BOTH tables, otherwise a secret rotation
-- would not propagate into the running snapshot (a compromised key would
-- keep working). Modeled on notify_block_write/ctx_block_write (M004 line) —
-- deliberately broader here: also DELETE, via COALESCE(NEW, OLD), for the
-- unset/revocation path. Payload carries ONLY identity+op, NEVER values or
-- ciphertext.
CREATE OR REPLACE FUNCTION notify_settings_write() RETURNS TRIGGER AS $$
DECLARE
    v_row JSONB := to_jsonb(COALESCE(NEW, OLD));
BEGIN
    PERFORM pg_notify('ctx_settings_write', json_build_object(
        'entity', TG_TABLE_NAME,
        'key',    COALESCE(v_row->>'key', v_row->>'name'),
        'op',     TG_OP)::text);
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

-- Audit in the trigger instead of the Go store layer: atomic with the
-- mutation and covers all write paths (API, psql, break-glass.sh
-- reset-settings). Actor comes from SET LOCAL ctx.api_key_id (API path);
-- psql edits => NULL + via='sql'. to_jsonb(NEW/OLD) holds ciphertext only
-- in a LOCAL variable for context_secrets — old/new reach the audit row
-- exclusively for settings (whose values are never sensitive, see the
-- secret_ref reject noted in the header).
CREATE OR REPLACE FUNCTION audit_settings_write() RETURNS TRIGGER AS $$
DECLARE
    v_new   JSONB := to_jsonb(NEW);
    v_old   JSONB := to_jsonb(OLD);
    v_actor UUID  := NULLIF(current_setting('ctx.api_key_id', true), '')::uuid;
    v_label TEXT;
BEGIN
    IF v_actor IS NOT NULL THEN
        SELECT label INTO v_label FROM context_api_keys WHERE id = v_actor;
    END IF;
    INSERT INTO context_settings_audit
        (entity_type, entity_key, scope, action, old_value, new_value,
         api_key_id, actor_label, metadata)
    VALUES (
        CASE TG_TABLE_NAME WHEN 'context_settings' THEN 'setting' ELSE 'secret' END,
        COALESCE(v_new->>'key', v_new->>'name', v_old->>'key', v_old->>'name'),
        COALESCE(v_new->>'scope', v_old->>'scope'),
        CASE WHEN TG_TABLE_NAME = 'context_settings'
             THEN CASE TG_OP WHEN 'DELETE' THEN 'unset' ELSE 'set' END
             ELSE CASE TG_OP WHEN 'INSERT' THEN 'create'
                             WHEN 'UPDATE' THEN 'rotate'
                             ELSE 'delete' END
        END,
        CASE WHEN TG_TABLE_NAME = 'context_settings' THEN v_old->'value' END,
        CASE WHEN TG_TABLE_NAME = 'context_settings' THEN v_new->'value' END,
        v_actor, v_label,
        jsonb_strip_nulls(jsonb_build_object(
            'via',        CASE WHEN v_actor IS NULL THEN 'sql' ELSE 'api' END,
            'request_id', NULLIF(current_setting('ctx.request_id', true), '')))
    );
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_settings_notify ON context_settings;
CREATE TRIGGER trg_settings_notify
    AFTER INSERT OR UPDATE OR DELETE ON context_settings
    FOR EACH ROW EXECUTE FUNCTION notify_settings_write();
DROP TRIGGER IF EXISTS trg_settings_audit ON context_settings;
CREATE TRIGGER trg_settings_audit
    AFTER INSERT OR UPDATE OR DELETE ON context_settings
    FOR EACH ROW EXECUTE FUNCTION audit_settings_write();

DROP TRIGGER IF EXISTS trg_secrets_notify ON context_secrets;
CREATE TRIGGER trg_secrets_notify
    AFTER INSERT OR UPDATE OR DELETE ON context_secrets
    FOR EACH ROW EXECUTE FUNCTION notify_settings_write();
DROP TRIGGER IF EXISTS trg_secrets_audit ON context_secrets;
CREATE TRIGGER trg_secrets_audit
    AFTER INSERT OR UPDATE OR DELETE ON context_secrets
    FOR EACH ROW EXECUTE FUNCTION audit_settings_write();

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (51, '051_settings_store.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 051_settings_store.sql

-- @@ ctx-fold begin 052_api_key_admin.sql
-- =============================================================================
-- 052_api_key_admin.sql — admin tier for context_api_keys + ctx_auth update
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Today every valid key of any home_scope can create/delete keys for ALL
-- scopes and flip dream-mode (HandleManage checks only IsValid). The settings/
-- secrets API must not inherit that model: a friend-tenant key setting
-- chat.host or injecting a provider secret is privilege escalation +
-- credential exfiltration. is_admin is the minimal cut; a role/permissions
-- model can layer on additively later (workflow-engine line).
--
-- home_scope VARCHAR(20)->VARCHAR(50): 047 widened context_blocks.scope for
-- multi-tenant scope names ('tenant:crag-research-q1' — its own target
-- examples exceed 20 chars) but left context_api_keys untouched. Without
-- this, no key can CARRY such a scope as home_scope (INSERT: value too
-- long) — per-tenant settings/secrets for those scopes would be unreachable.
-- 052 is the documented DROP+CREATE opportunity for ctx_auth (return type +
-- internal variables enforce the typmod), so the widening happens HERE, not
-- in a later migration that would have to DROP+CREATE ctx_auth again.
-- Widening is metadata-only (047 line). Remaining VARCHAR(20) scope columns
-- elsewhere (blobs, write_log, ingestion_sources, 016, fn signatures
-- 006/011/030) = known follow-up migration outside F2.
--
-- ctx_auth returns a TABLE — adding a column changes the return type, which
-- CREATE OR REPLACE refuses ("cannot change return type"). DROP + CREATE in
-- one tx; migrations run before server start, so there is no live-traffic
-- window within the same process. Old binaries keep working against the new
-- function because auth.go selects named columns, never SELECT *.
--
-- No key is auto-promoted: bootstrap is a documented one-time SQL per key
-- id (host access == full trust anyway; label is NOT unique and would
-- escalate every same-labeled key). See README "Admin bootstrap". The
-- eval/test script keys and the MCP/OAuth token key deliberately stay
-- non-admin (least privilege; the OAuth flow hands the api key itself out
-- as bearer token).
-- =============================================================================

ALTER TABLE context_api_keys
    ADD COLUMN IF NOT EXISTS is_admin BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE context_api_keys
    ALTER COLUMN home_scope TYPE VARCHAR(50);

DROP FUNCTION IF EXISTS ctx_auth(TEXT);

CREATE FUNCTION ctx_auth(p_api_key TEXT)
RETURNS TABLE (
    api_key_id UUID,
    home_scope VARCHAR(50),
    allowed_scopes TEXT[],
    read_scopes TEXT[],
    is_valid BOOLEAN,
    is_admin BOOLEAN
) LANGUAGE plpgsql AS $$
DECLARE
    v_key_hash TEXT;
    v_api_key_id UUID;
    -- VARCHAR(50): PL/pgSQL enforces the typmod on assignment — an internal
    -- VARCHAR(20) variable would re-truncate what the column widening allows.
    v_home_scope VARCHAR(50);
    v_allowed_scopes TEXT[];
    v_is_admin BOOLEAN;
BEGIN
    -- Compute SHA-256 hash of the provided API key
    v_key_hash := encode(digest(p_api_key, 'sha256'), 'hex');

    -- Authenticate: update last_used_at and return key info
    UPDATE context_api_keys
    SET last_used_at = now()
    WHERE context_api_keys.key_hash = v_key_hash
      AND context_api_keys.active = true
    RETURNING
        context_api_keys.id,
        context_api_keys.home_scope,
        context_api_keys.allowed_scopes,
        context_api_keys.is_admin
    INTO v_api_key_id, v_home_scope, v_allowed_scopes, v_is_admin;

    -- Check if we found a valid key
    IF v_api_key_id IS NULL THEN
        -- Invalid key: return sentinel values
        api_key_id     := NULL;
        home_scope     := '__UNAUTHORIZED__';
        allowed_scopes := '{}'::TEXT[];
        read_scopes    := '{}'::TEXT[];
        is_valid       := false;
        is_admin       := false;
        RETURN NEXT;
        RETURN;
    END IF;

    -- Valid key: build read_scopes = [home_scope] || allowed_scopes
    api_key_id     := v_api_key_id;
    home_scope     := v_home_scope;
    allowed_scopes := v_allowed_scopes;
    read_scopes    := ARRAY[v_home_scope::TEXT] || COALESCE(v_allowed_scopes, '{}'::TEXT[]);
    is_valid       := true;
    is_admin       := v_is_admin;
    RETURN NEXT;
    RETURN;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (52, '052_api_key_admin.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 052_api_key_admin.sql

-- @@ ctx-fold begin 053_backend_pool.sql
-- =============================================================================
-- 053_backend_pool.sql — deklarativer LLM-Backend-Pool mit Trust-Stufen (F3-P1)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Ersetzt das hartverdrahtete Paar CTX_CHAT_HOST + CTX_CHAT_FALLBACK_* durch
-- eine Rolle→Backends-Routing-Tabelle. Trust-Stufen gaten, welcher Content
-- (context_blocks.sensitivity, Migration 055) ein Backend erreichen darf.
-- Secrets liegen NICHT hier: api_key_ref referenziert ein F2-Secret per Name
-- (context_secrets); die Tabelle trägt damit by construction kein Key-Material.
-- Reihenfolge/Priorität sind reine DATEN (E2: kein Magic-Value-Pattern) —
-- der Go-Bootstrap schreibt nur Initialwerte für die heutigen env-Backends.
-- limits: reservierte Naht für F6 (K7: chat_max_tokens der CPU-Klasse) und
-- die Kapazitäts-Welle (metadata: max_parallel, ctx_per_slot).
-- =============================================================================

CREATE TABLE IF NOT EXISTS context_backends (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    -- TODO(multi-tenant): pool visibility — today every backend row is
    -- server-global; per-tenant pools/quotas need a scope dimension here.
    name            TEXT NOT NULL,
    base_url        TEXT NOT NULL,
    protocol        TEXT NOT NULL DEFAULT 'openai'
                    CHECK (protocol IN ('openai','ollama','rerank')),
    provider_class  TEXT NOT NULL DEFAULT 'generic'
                    CHECK (provider_class IN ('generic','llamacpp','openrouter')),
    api_key_ref     TEXT,                                -- F2-Secret-Name; NULL = keyless (lokal)
    -- trust/sensitivity gehen in einen numerischen Rang-Vergleich: ein
    -- unbekannter Wert wäre kein "neuer Tenant-Wert", sondern ein kaputtes
    -- Gate — CHECK bewusst hart (wie block_role, anders als scope).
    trust           TEXT NOT NULL DEFAULT 'public'       -- fail-closed: neue Backends explizit hochstufen
                    CHECK (trust IN ('full-trust','no-credentials','non-personal','public')),
    locality        TEXT NOT NULL DEFAULT 'external'     -- Egress-Audit-Dimension; NICHT frei wählbar:
                    CHECK (locality IN ('local','lan','external')),  -- gegen base_url validiert (Go)
    roles           TEXT[] NOT NULL DEFAULT '{}',        -- synthesis|translate|embed|rerank|dream|digest|chat|frei
    model_map       JSONB NOT NULL DEFAULT '{}',         -- Rolle→ModelSpec: "modell-id" (Kurzform) ODER
                                                         -- {"model":"…","params":{"top_p":0.8,…}}
    timeouts        JSONB NOT NULL DEFAULT '{}',         -- {"synthesis":420} Sekunden; fehlend = Code-Default der Rolle
    num_ctx         INTEGER,                             -- nur ollama-Protokoll wire-relevant
    priority        INTEGER NOT NULL DEFAULT 0,          -- höher = bevorzugt
    enabled         BOOLEAN NOT NULL DEFAULT true,
    extra_headers   JSONB NOT NULL DEFAULT '{}',         -- z. B. HTTP-Referer/X-Title; Denylist-validiert (Go):
                                                         -- KEINE Credential-Header
    extra_body      JSONB NOT NULL DEFAULT '{}',         -- z. B. {"provider":{"require_parameters":true}};
                                                         -- zdr/deny wird bei provider_class=openrouter IMMER
                                                         -- erzwungen (trust-UNABHÄNGIG), Feld kann nur verschärfen
    limits          JSONB NOT NULL DEFAULT '{}',         -- K7-Naht: chat_max_tokens (F6-C2 CPU-Klasse)
    metadata        JSONB NOT NULL DEFAULT '{}',         -- reservierte Keys: max_parallel, ctx_per_slot,
                                                         -- embed_equivalence_verified, allow_data_collection
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_backends_name UNIQUE (name)
);

-- Hot-Reload: Backend-Mutationen müssen den Pool-Snapshot OHNE Restart
-- erreichen (User-Kernanforderung "on-the-fly"; deckt auch Break-Glass-
-- psql-Edits). Gleicher Kanal + gleiche Funktion wie 051: die bestehende
-- notify_settings_write() liest entity=TG_TABLE_NAME und den Namen via
-- COALESCE(key, name) — context_backends.name passt ohne Anpassung. Der
-- Go-Listener dispatcht auf entity='context_backends' → Pool-Reload.
DROP TRIGGER IF EXISTS trg_backends_notify ON context_backends;
CREATE TRIGGER trg_backends_notify
    AFTER INSERT OR UPDATE OR DELETE ON context_backends
    FOR EACH ROW EXECUTE FUNCTION notify_settings_write();

-- Audit im Trigger (Muster 051): atomar mit der Mutation, deckt API, psql
-- und break-glass gleichermaßen. Defense-in-Depth: extra_headers-WERTE
-- werden redacted, bevor old/new in die append-only Audit-Tabelle gehen —
-- falls die Go-Denylist je ein Credential-Carrier-Pattern verpasst, liegt
-- der Wert wenigstens nicht für immer im Audit (Header-NAMEN bleiben
-- sichtbar: das Audit zeigt WELCHE Header sich änderten, nie Werte).
CREATE OR REPLACE FUNCTION audit_backends_write() RETURNS TRIGGER AS $$
DECLARE
    v_new   JSONB := to_jsonb(NEW);
    v_old   JSONB := to_jsonb(OLD);
    v_actor UUID  := NULLIF(current_setting('ctx.api_key_id', true), '')::uuid;
    v_label TEXT;
BEGIN
    IF v_new ? 'extra_headers' THEN
        SELECT jsonb_set(v_new, '{extra_headers}',
                         COALESCE(jsonb_object_agg(k, '"[redacted]"'::jsonb), '{}'::jsonb))
          INTO v_new
          FROM jsonb_object_keys(v_new->'extra_headers') AS k;
    END IF;
    IF v_old ? 'extra_headers' THEN
        SELECT jsonb_set(v_old, '{extra_headers}',
                         COALESCE(jsonb_object_agg(k, '"[redacted]"'::jsonb), '{}'::jsonb))
          INTO v_old
          FROM jsonb_object_keys(v_old->'extra_headers') AS k;
    END IF;
    IF v_actor IS NOT NULL THEN
        SELECT label INTO v_label FROM context_api_keys WHERE id = v_actor;
    END IF;
    INSERT INTO context_settings_audit
        (entity_type, entity_key, scope, action, old_value, new_value,
         api_key_id, actor_label, metadata)
    VALUES (
        'backend',
        COALESCE(v_new->>'name', v_old->>'name'),
        '_global',
        CASE TG_OP WHEN 'INSERT' THEN 'create'
                   WHEN 'UPDATE' THEN 'update'
                   ELSE 'delete' END,
        v_old, v_new,
        v_actor, v_label,
        jsonb_strip_nulls(jsonb_build_object(
            'via',        CASE WHEN v_actor IS NULL THEN 'sql' ELSE 'api' END,
            'request_id', NULLIF(current_setting('ctx.request_id', true), '')))
    );
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_backends_audit ON context_backends;
CREATE TRIGGER trg_backends_audit
    AFTER INSERT OR UPDATE OR DELETE ON context_backends
    FOR EACH ROW EXECUTE FUNCTION audit_backends_write();

INSERT INTO _migrations (version, filename, applied_at)
VALUES (53, '053_backend_pool.sql', now()) ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 053_backend_pool.sql

-- @@ ctx-fold begin 054_llmlog_backend_telemetry.sql
-- =============================================================================
-- 054_llmlog_backend_telemetry.sql — Provenance + Trust + Kosten im LLM-Log (F3-P2)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Fixt die Fallback-Blindstelle (host=Primary bei usedFallback) strukturell:
-- backend_name = tatsächlich antwortendes Backend; metadata.chain trägt alle
-- Versuche. cost_usd aus OpenRouter usage.cost (Response-Body, G29); lokale
-- Backends NULL. Egress-Audit = partial Index auf backend_locality='external'
-- — zusammen mit der bestehenden block_ids-Spalte (M025) ist die Egress-Spur
-- ID-genau rekonstruierbar, auch für künftige body-lose Slim-Zeilen (P3/P4).
-- api_key_id (K7/E4): Caller-Attribution — die letzte günstige Migration für
-- das Proxy-Accounting-Fundament; bewusst KEIN FK (Log referenziert, Key-
-- Delete darf Historie nicht anonymisieren — 051-Audit-Linie).
-- =============================================================================

ALTER TABLE context_llm_log
    ADD COLUMN IF NOT EXISTS backend_name         TEXT,
    ADD COLUMN IF NOT EXISTS backend_trust        TEXT,
    ADD COLUMN IF NOT EXISTS backend_locality     TEXT,
    ADD COLUMN IF NOT EXISTS required_sensitivity TEXT,
    ADD COLUMN IF NOT EXISTS attempt              SMALLINT,
    ADD COLUMN IF NOT EXISTS cost_usd             NUMERIC(14,8),
    ADD COLUMN IF NOT EXISTS api_key_id           UUID;

CREATE INDEX IF NOT EXISTS idx_llm_log_backend
    ON context_llm_log (backend_name, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_llm_log_external
    ON context_llm_log (created_at DESC) WHERE backend_locality = 'external';

INSERT INTO _migrations (version, filename, applied_at)
VALUES (54, '054_llmlog_backend_telemetry.sql', now()) ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 054_llmlog_backend_telemetry.sql

-- @@ ctx-fold begin 055_block_sensitivity.sql
-- =============================================================================
-- 055_block_sensitivity.sql — Content-Sensitivität pro Block (F3-P3 Trust-Gating)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Default 'credentials' = fail-closed (E1): unklassifizierte Blöcke erreichen
-- ausschließlich full-trust-Backends; der Normalbetrieb (lokal + LAN, alle
-- full-trust) ist davon unberührt, nur das künftige externe Netz (G29) bleibt
-- dunkel, bis klassifiziert ist. PG18: ADD COLUMN mit non-volatile DEFAULT
-- ist metadata-only (kein Rewrite) — gilt auch bei 1M+ Zeilen.
-- CHECK bewusst hart (wie block_role, anders als das generalisierte scope):
-- die Stufe geht in einen numerischen Rang-Vergleich — ein unbekannter Wert
-- wäre kein "neuer Tenant-Wert", sondern ein kaputtes Gate.
-- sensitivity_source + sensitivity_audited_at (K7, G41-Naht): der LLM-Audit
-- klassifiziert nur source='default'-Blöcke; 'manual' ist unantastbar;
-- audited_at trägt Idempotenz + Re-Audit-Fähigkeit.
-- =============================================================================

ALTER TABLE context_blocks
    ADD COLUMN IF NOT EXISTS sensitivity TEXT NOT NULL DEFAULT 'credentials'
        CHECK (sensitivity IN ('credentials','personal','internal','public')),
    ADD COLUMN IF NOT EXISTS sensitivity_source TEXT NOT NULL DEFAULT 'default'
        CHECK (sensitivity_source IN ('default','llm-audit','pattern','manual')),
    ADD COLUMN IF NOT EXISTS sensitivity_audited_at TIMESTAMPTZ;

-- Klassifizierungs-Fortschritt für UI/Stats (count GROUP BY sensitivity):
CREATE INDEX IF NOT EXISTS idx_blocks_sensitivity
    ON context_blocks (sensitivity) WHERE NOT is_archived;

INSERT INTO _migrations (version, filename, applied_at)
VALUES (55, '055_block_sensitivity.sql', now()) ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 055_block_sensitivity.sql

-- @@ ctx-fold begin 056_chat_sessions.sql
-- =============================================================================
-- 056_chat_sessions.sql — Web-Chat Sessions + Messages (F6-C2/G35)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Persistente Chat-Sessions (Design 06 §3.1/§3.10): in-memory stürbe bei jedem
-- Wave-Deploy, und ctxd bleibt bewusst stateless-restartbar.
--
-- Zwei Sichtbarkeits-Achsen, BEWUSST getrennt (06 §3.1):
--   * Ownership = scope (home_scope des anlegenden Keys). Liste + DELETE sind
--     home_scope-weit — Key-Rotation zerstört keine Sessions, created_by bleibt
--     nur als Audit-Pointer (SET-NULL wie access_log).
--   * read_scopes = Snapshot der ReadScopes des anlegenden Keys. Tool-Results
--     können Cross-Scope-Content tragen (private liest hth, live existent) und
--     werden ungekürzt persistiert; Detail-Lesen + Fortsetzen erfordern daher
--     read_scopes ⊆ caller.ReadScopes (sonst 404) — gegen den Schatten-Korpus-
--     Kanal (least-privilege-Agent-Keys der Workflow-Engine-Linie).
--
-- max_sensitivity = High-Water-Mark des Session-Contents, MONOTON steigend
-- (06 §2.3, F3 §2.2): ein credentials-Tool-Result aus Turn 1 steckt in jedem
-- Folge-Turn-Prompt — das Backend-Gate misst über die HWM, nicht über
-- "aktuelle" Inhalte. 'public' ist nur der Anlage-Zustand; AppendMessage hebt
-- die HWM in DERSELBEN TX mit JEDER Message auf max(bisher, msg.sensitivity).
--
-- CHECKs bewusst hart (wie block_role/block_sensitivity, anders als das
-- generalisierte scope): die Stufe geht in einen numerischen Rang-Vergleich
-- (backends.sensRank) — ein unbekannter Wert wäre kein neuer Tenant-Wert,
-- sondern ein kaputtes Gate. message.sensitivity DEFAULT 'credentials' ist
-- fail-closed (06 §2.3 required-Berechnung).
--
-- Busy-Mechanik statt Turn-langem Row-Lock (06 §3.1): ein Turn dauert
-- LLM-Latenz (GPU ~47s, CPU bis 900s) — eine TX so lange offen zu halten
-- sättigte den pgxpool (MaxConns=20) und hielte den xmin-Horizont gegen
-- Autovacuum. busy_until = kurze CAS-TX; abgelaufen = frei (Crash heilt sich).
-- =============================================================================

CREATE TABLE IF NOT EXISTS context_chat_sessions (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    scope           VARCHAR(50) NOT NULL,
    read_scopes     TEXT[] NOT NULL,                 -- Snapshot ar.ReadScopes bei Anlage
    created_by      UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,
    title           VARCHAR(200) NOT NULL DEFAULT 'New chat',
    max_sensitivity TEXT NOT NULL DEFAULT 'public'
                    CHECK (max_sensitivity IN ('credentials','personal','internal','public')),
    busy_until      TIMESTAMPTZ,                     -- aktiver Turn (NULL/abgelaufen = frei)
    metadata        JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_chat_sessions_scope
    ON context_chat_sessions (scope, updated_at DESC) WHERE archived_at IS NULL;

CREATE TABLE IF NOT EXISTS context_chat_messages (
    id            UUID PRIMARY KEY DEFAULT uuidv7(),
    session_id    UUID NOT NULL REFERENCES context_chat_sessions(id) ON DELETE CASCADE,
    seq           INTEGER NOT NULL,
    role          VARCHAR(20) NOT NULL
                  CHECK (role IN ('user','assistant','tool')),
    content       TEXT NOT NULL DEFAULT '',
    sensitivity   TEXT NOT NULL DEFAULT 'credentials'   -- fail-closed; §2.3 required-Berechnung
                  CHECK (sensitivity IN ('credentials','personal','internal','public')),
    tool_calls    JSONB,            -- assistant: [{id,name,arguments}]
    tool_call_id  VARCHAR(64),      -- tool: Korrelation
    tool_name     VARCHAR(64),      -- tool: ctx_query|ctx_search|ctx_get|ctx_recent
    backend       VARCHAR(100),     -- F3-Backend-Name, der den Call bediente
    model         VARCHAR(200),
    prompt_tokens     INTEGER,
    completion_tokens INTEGER,
    duration_ms   INTEGER,
    metadata      JSONB NOT NULL DEFAULT '{}',  -- {canceled:true, iteration:N, truncated:…}
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (session_id, seq)
);
-- KEIN expliziter Index auf (session_id, seq): der UNIQUE-Constraint legt genau
-- diesen B-Tree bereits an und bedient ORDER BY seq-Reads — ein zweiter wäre
-- reine doppelte Write-Last pro INSERT.

INSERT INTO _migrations (version, filename, applied_at)
VALUES (56, '056_chat_sessions.sql', now()) ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 056_chat_sessions.sql

-- @@ ctx-fold begin 057_graph_overview.sql
-- =============================================================================
-- 057_graph_overview.sql — F5-W6 Übersichts-Landkarte (Louvain-Cluster-Supergraph)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Vorberechnete Louvain-Cluster über context_dream_links als Cluster-Supergraph
-- (Design 07-graph-overview.md). Drei vom Daemon-Job (internal/overview) per
-- TRUNCATE+INSERT in EINER Tx befüllte Tabellen — KEINE Materialized View:
--   * der Migrations-Runner hält jede Datei in EINER Tx (migrations.go:87) →
--     REFRESH ... CONCURRENTLY rollte 057 zurück;
--   * WITH NO DATA wäre bis zum ersten Refresh nicht abfragbar (Boot-Falle).
-- Normale Tabellen sind nach der Migration sofort abfragbar (leer = leere
-- Landkarte, kein Fehler), bis der erste Rebuild-Tick läuft.
--
-- SCOPE-INVARIANTE (der gelöste Count-Leak, Design §2): die Aggregate sind PRO
-- SCOPE partitioniert. graph_cluster_node trägt eine Zeile je (cluster, scope),
-- graph_cluster_edge eine je (cluster-paar, scope-paar). Der Read-Pfad (W2)
-- summiert NUR Zeilen mit scope = ANY(readScopes) (Kanten: BEIDE Endpunkt-
-- Scopes sichtbar, wie inducedEdges). Es existiert nie ein globaler Total, aus
-- dem sich per Differenz ein fremder privater Anteil rekonstruieren ließe.
-- Sichtbarkeit gilt gegen context_blocks.scope, NIE context_dream_links.scope
-- (Promotion lässt l.scope abweichen — visibility.go:21).
--
-- cluster_id = kleinste Member-UUID der Community (inhaltsstabil bei identischem
-- Korpus; NICHT der gonum-Slice-Index, der über Läufe instabil ist). cluster_id
-- ist ein interner GROUP-BY-Key und wird NIE roh an einen Tenant ausgeliefert
-- (Existenz-Orakel, Design §6.1) — der Handler vergibt per Request einen dichten
-- Ordinal.
-- =============================================================================

-- (A) Block → Cluster-Zuordnung.
CREATE TABLE IF NOT EXISTS graph_cluster_member (
    block_id    UUID PRIMARY KEY REFERENCES context_blocks(id) ON DELETE CASCADE,
    cluster_id  UUID NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_gcm_cluster ON graph_cluster_member (cluster_id);

-- (B) Meta-Knoten-Aggregate, PARTITIONIERT NACH SCOPE.
--     size/category_counts/repr nur über Member dieses einen Scopes.
CREATE TABLE IF NOT EXISTS graph_cluster_node (
    cluster_id       UUID    NOT NULL,
    scope            TEXT    NOT NULL,
    size             INT     NOT NULL,
    category_counts  JSONB   NOT NULL DEFAULT '{}',
    repr_block_id    UUID    NOT NULL,
    repr_title       VARCHAR(120) NOT NULL,
    repr_quality     REAL    NOT NULL DEFAULT 0,
    PRIMARY KEY (cluster_id, scope)
);
CREATE INDEX IF NOT EXISTS idx_gcn_scope ON graph_cluster_node (scope);

-- (C) Inter-Cluster-Meta-Kanten-Aggregate, PARTITIONIERT NACH SCOPE-PAAR.
--     Ungerichtet normiert (cluster_a < cluster_b). scope_s/scope_t = Scopes
--     der beiden Endpunkt-BLÖCKE. Sichtbar gdw. BEIDE Scopes in readScopes.
CREATE TABLE IF NOT EXISTS graph_cluster_edge (
    cluster_a   UUID NOT NULL,
    cluster_b   UUID NOT NULL,
    scope_s     TEXT NOT NULL,
    scope_t     TEXT NOT NULL,
    link_count  INT  NOT NULL,
    weight_sum  REAL NOT NULL,
    PRIMARY KEY (cluster_a, cluster_b, scope_s, scope_t),
    CHECK (cluster_a < cluster_b)
);
CREATE INDEX IF NOT EXISTS idx_gce_scopes ON graph_cluster_edge (scope_s, scope_t);

-- (D) Metadaten des letzten Rebuilds (Single-Row; ETag-Kandidat + Q-Score-Smoke).
CREATE TABLE IF NOT EXISTS graph_overview_meta (
    singleton   BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
    computed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    modularity  REAL NOT NULL DEFAULT 0,
    cluster_n   INT  NOT NULL DEFAULT 0,
    node_n      INT  NOT NULL DEFAULT 0,
    edge_n      INT  NOT NULL DEFAULT 0,
    resolution  REAL NOT NULL DEFAULT 1.0
);

INSERT INTO _migrations (version, filename, applied_at)
VALUES (57, '057_graph_overview.sql', now()) ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 057_graph_overview.sql

-- @@ ctx-fold begin 058_scope_generalize.sql
-- =============================================================================
-- 058_scope_generalize.sql — legacy scope generalization on the 4 remaining tables
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Multi-Tenant wave T01 (Achse 01). 045 dropped chk_scope and 047 widened
-- context_blocks.scope to VARCHAR(50); 052 did the same for
-- context_api_keys.home_scope. 052:20-22 names the explicit known follow-up:
-- the remaining VARCHAR(20) scope columns on blobs / write_log /
-- ingestion_sources / 016 (dream_links) plus the two surviving 3-value
-- CHECKs (chk_blob_scope, chk_source_scope). This migration is that follow-up
-- for the TABLE COLUMNS + the 2 CHECKs only — so arbitrary, tenant-prefixed
-- scope names ('tenant:crag-research-q1', 'org-1234567:archive') become
-- storable on every data table, consistent with context_blocks.
--
-- Scope of 058 (deliberately narrow): table scope columns + the 2 CHECKs.
-- NOT touched here: the ctx_rrf / ctx_auth-family function signatures that
-- still declare scope/home_scope VARCHAR(20) in their RETURNS TABLE / DECLARE
-- blocks (006/011/030/033/036/037/038/039, 003/007/018/020). 052:20-22 marks
-- "fn signatures 006/011/030" as a SEPARATE later wave because widening a
-- function return type requires DROP FUNCTION + CREATE (CREATE OR REPLACE
-- refuses a changed return type, see 052's ctx_auth note). 058 is metadata
-- on tables only and intentionally does not reopen those functions.
--
-- No consumer: with no tenant emitting non-legacy scope strings, this is
-- behaviorally identical to today (the widened columns still hold every
-- existing ≤20-char value; the dropped CHECKs were the only thing that would
-- have rejected a new short non-legacy value). Pausable / inert until a later
-- wave actually writes tenant-prefixed scopes.
--
-- Properties: ALTER COLUMN ... TYPE VARCHAR(50) is metadata-only when widening
-- (no table rewrite, no row re-validation; 047 line). All existing scope
-- values (≤20 chars) remain valid. DROP CONSTRAINT removes a row-level CHECK,
-- never rewrites.
--
-- lock_timeout (R-MIG2): a metadata-only typmod widening is NOT a rewrite, but
-- ALTER TABLE still acquires ACCESS EXCLUSIVE briefly. At 1M-row scale a
-- concurrent long-running transaction could park that lock request at the head
-- of the lock queue and stall readers behind it. 047 set no lock_timeout, but
-- the migration runner (internal/store/migrations.go) wraps EACH migration in
-- its own real transaction, so SET LOCAL here is transaction-scoped and
-- self-reverting — it does not leak to the session or to migration 059+. We
-- prefer fail-fast (the migration aborts cleanly, rolls back, and is simply
-- re-runnable) over an unbounded ACCESS EXCLUSIVE wait that could freeze these
-- tables behind a slow reader. Strictly safer than 047, no downside for the
-- empty-grant / no-consumer case here. 3s is generous for a metadata-only DDL.
SET LOCAL lock_timeout = '3s';
--
-- Idempotent: DROP CONSTRAINT IF EXISTS + ALTER TYPE is a no-op when the
-- column already is VARCHAR(50); _migrations PK + ON CONFLICT guards the row.
-- Reversible: ALTER COLUMN scope TYPE VARCHAR(20) + re-ADD the CHECKs —
-- narrowing fails if any row has scope >20 chars or a non-legacy value after
-- roll-forward (expected once tenants exist; this is forward-only in practice).
-- =============================================================================

-- 1. Drop the 2 surviving 3-value scope CHECKs (blocks' chk_scope already gone via 045).
ALTER TABLE context_blobs   DROP CONSTRAINT IF EXISTS chk_blob_scope;
ALTER TABLE context_sources DROP CONSTRAINT IF EXISTS chk_source_scope;

-- 2. Widen the 4 remaining VARCHAR(20) scope columns to VARCHAR(50)
--    (context_blocks 047 + context_api_keys.home_scope 052 are already 50).
ALTER TABLE context_blobs       ALTER COLUMN scope TYPE VARCHAR(50);  -- 001_initial.sql:103
ALTER TABLE context_sources     ALTER COLUMN scope TYPE VARCHAR(50);  -- 012_ingestion_sources.sql:19
ALTER TABLE context_dream_links ALTER COLUMN scope TYPE VARCHAR(50);  -- 016_dream.sql:22
ALTER TABLE context_write_log   ALTER COLUMN scope TYPE VARCHAR(50);  -- 001_initial.sql:130

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (58, '058_scope_generalize.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 058_scope_generalize.sql

-- @@ ctx-fold begin 059_tenants_hybrid.sql
-- =============================================================================
-- 059_tenants_hybrid.sql — Tenant owner-register + scope partition map (E1, Modell C)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Multi-Tenant wave T02 (Achse 01, Tenant-Identität). Builds on 058
-- (scope-Generalisierung). User-decided tenant model = C (hybrid, E1):
-- context_tenants is the OWNER/management register (lifecycle, display_name,
-- status, quotas, admin assignment), but the data-bearing tables get NO
-- tenant_id — scope STAYS the data discriminator (like model B), so there is
-- NO constraint-swap and NO tenant_id backfill on the 1M+ tables
-- (context_blocks / dream_links / blobs / sources are NOT touched here; that
-- would be model A). The bridge is a narrow partition map: which scopes belong
-- to which tenant. Only context_api_keys (small table, ~7 rows) gains a slim
-- tenant_id FK (which owner holds the key) plus the tenant_role bootstrap.
--
-- Exact schema vorlage: design/01-tenant-model.md §3.4 (Z.325-362), erweitert
-- um die tenant_role-Spalte (Masterplan T02 + design/05-admin-auth-mt.md §3.1
-- K4: der tenant_role-CHECK gehört in DIESE Achse-01-Migration, die tenant_id
-- einführt — NICHT in eine separate 058 der Achse 05, die wurde gestrichen).
--
-- TENANT-DECISION(tenant-role-domain): owner|admin|member (3-Tier RBAC-Bootstrap)
--   — Alt member|tenant-admin (2-Tier), umentscheidbar via additive CHECK-
--   Erweiterung. owner verwaltet+delegiert (ernennt admins, OE-2), admin
--   verwaltet, member nutzt. Dieser Wertebereich ist BYTE-IDENTISCH mit dem
--   Go-Role-Typ (T20) + dem whoami-role-Wire-Contract (T21) zu halten (§9 K4).
--   RBAC-Pfad: Permission-Checks laufen perspektivisch via auth.Can(ar,Perm,
--   tenant) NICHT über den role-String (L2/T20); roles/permissions/
--   api_key_roles-Tabellen sind eine eigene Welle (L3), in der tenant_role zur
--   Default-Rolle / zum role_id-FK wird. 059 bereitet RBAC nur KOMMENTARISCH
--   vor (dieser Marker + die Spalte) und baut RBAC NICHT.
--
-- TENANT-DECISION(default-tenant-slug): 'default' (E9) — kosmetisch, die feste
--   UUID 00000000-0000-0000-0000-0000000d3fa0 ('...d3fa0' ≈ 'default') trägt die
--   Identität und ist das Join-Ziel für jeden Bestands-scope und -Key.
--   Umentscheidbar (nur Backfill-Konstante), die UUID bleibt.
--
-- lock_timeout (R-MIG2): die ALTER TABLE ... ADD COLUMN auf context_api_keys
-- nimmt kurz ACCESS EXCLUSIVE. Der Runner (internal/store/migrations.go) wickelt
-- JEDE Migration in ihre eigene reale Transaktion, daher ist SET LOCAL hier
-- transaktions-gescopt und selbst-revertierend — es leakt nicht in die Session
-- oder nach 060+. Wir bevorzugen fail-fast (Migration bricht sauber ab, rollt
-- zurück, ist einfach re-runnable) gegenüber unbegrenztem Lock-Warten hinter
-- einem langsamen Reader. ADD COLUMN ... DEFAULT 'member' ist ein
-- Metadaten-Default (kein Rewrite seit PG11), 3s ist großzügig.
SET LOCAL lock_timeout = '3s';
--
-- Idempotent: alle CREATE TABLE IF NOT EXISTS / ADD COLUMN IF NOT EXISTS /
-- ON CONFLICT DO NOTHING (slug bzw. scope bzw. version PK). Der Backfill-UPDATE
-- ist spalten-agnostisch und no-op beim 2. Lauf (kein NULL tenant_id mehr).
-- Reversibel: DROP COLUMN tenant_role/tenant_id + DROP TABLE
-- context_tenant_scopes/context_tenants (forward-only in der Praxis).
-- =============================================================================

-- 1. context_tenants — Owner-/Verwaltungs-Registratur (Lifecycle, Status, Quotas).
CREATE TABLE IF NOT EXISTS context_tenants (
    id           UUID PRIMARY KEY DEFAULT uuidv7(),  -- PG18 nativ (001_initial.sql:58)
    slug         VARCHAR(50) NOT NULL,
    display_name TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active','suspended','offboarding')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata     JSONB NOT NULL DEFAULT '{}',
    CONSTRAINT uq_tenants_slug UNIQUE (slug)
);

-- 2. Default-Tenant: feste UUID trägt die Identität (slug ist kosmetisch).
INSERT INTO context_tenants (id, slug, display_name)
  VALUES ('00000000-0000-0000-0000-0000000d3fa0', 'default', 'Default Tenant')
  ON CONFLICT (slug) DO NOTHING;

-- 3. context_tenant_scopes — scope -> tenant (Partition: EIN scope = EIN Tenant).
--    Gibt der Visibility-Achse die per-Tenant-scope-Auflösung, ohne dass die
--    Daten-Tabellen tenant_id tragen.
CREATE TABLE IF NOT EXISTS context_tenant_scopes (
    scope     VARCHAR(50) PRIMARY KEY,          -- ein scope = ein Tenant
    tenant_id UUID NOT NULL REFERENCES context_tenants(id) ON DELETE CASCADE
);

-- 4. Bestands-scopes {private, work, shared} -> default-Tenant. _global
--    (settings/secrets) bleibt System, gehört keinem Tenant — daher NICHT hier
--    (eine fehlende Zeile = "kein Tenant" = System, fail-closed).
INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES
  ('private', '00000000-0000-0000-0000-0000000d3fa0'),
  ('work',    '00000000-0000-0000-0000-0000000d3fa0'),
  ('shared',  '00000000-0000-0000-0000-0000000d3fa0')
  ON CONFLICT (scope) DO NOTHING;

-- 5. Schlanker FK von api_keys auf den Tenant (welcher Owner besitzt den Key).
--    KEINE FK auf context_blocks etc. — deren Tenant ist via scope ableitbar.
--    DEFAULT = default-Tenant: jeder NEU gemintete Key (CreateApiKey setzt
--    tenant_id heute noch nicht — der +tenantID-Param kommt erst T06) landet
--    im default-Tenant (volle private/work/shared-Sicht) statt NULL — ein NULL-
--    Tenant ergäbe eine degradierte read_scopes-Auflösung. Das ist die KORREKTE
--    single-tenant-Übergangs-Semantik, NICHT fail-closed im MT-Sinn: der default-
--    Tenant ist ein REALER Tenant, nicht die Minimal-Sicht (≠ tenant_role 'member',
--    das echt fail-closed ist). PFLICHT T06 (MT-fail-loud): CreateApiKey setzt
--    tenant_id explizit + SET NOT NULL + DROP DEFAULT — sonst landet ein
--    vergessenes Fremd-Tenant-tenant_id still im default-Tenant (dann fail-OPEN).
--    // TENANT-DECISION(api-key-tenant-default): default-UUID-DEFAULT nur Übergang.
--    ON DELETE RESTRICT: ein nacktes Tenant-DELETE wird BLOCKIERT, solange Keys
--    zeigen (23503). Tenant-Lifecycle (User 2026-06-15): "zu gehen" = status
--    'suspended' (stumm — ctx_auth -> __UNAUTHORIZED__ + Background/Dream stoppt
--    für die scopes des Tenants, keine LLM-Kosten für Nicht-Zahler); DATEN bleiben
--    unangetastet -> voll reaktivierbar (status zurück auf 'active'). Die echte
--    Daten-Löschung (full-prune) ist eine EXPLIZITE, geordnete server-admin-Action
--    (Achse 01 / T05 tenant-prune), NIE ein implizites Cascade/Orphan — RESTRICT
--    erzwingt den geordneten Weg (erst Keys/Daten räumen, dann Tenant).
--    context_tenant_scopes behält CASCADE (schmale Zuordnung, keine Daten).
--    // TENANT-DECISION(tenant-delete-fk): RESTRICT (kontrollierter Super-Admin-Prune).
ALTER TABLE context_api_keys ADD COLUMN IF NOT EXISTS tenant_id UUID
    DEFAULT '00000000-0000-0000-0000-0000000d3fa0' REFERENCES context_tenants(id) ON DELETE RESTRICT;

-- 6. tenant_role — RBAC L1-Bootstrap (E2). DEFAULT 'member' = fail-closed
--    (neuer Key trägt keine Verwaltungs-/Delegations-Macht). Orthogonal zu
--    is_admin (052: server-weit) — tenant_role gilt INNERHALB des Tenants.
--    CHECK-Wertebereich siehe TENANT-DECISION(tenant-role-domain) im Header.
ALTER TABLE context_api_keys ADD COLUMN IF NOT EXISTS tenant_role TEXT NOT NULL DEFAULT 'member'
    CHECK (tenant_role IN ('owner','admin','member'));

-- 7. Backfill: Bestands-Keys -> default-Tenant. tenant_role bleibt DEFAULT
--    'member' (KEIN Auto-Promote analog is_admin-Bootstrap 052:30-35 —
--    fail-closed). Der UPDATE ist no-op beim 2. Lauf (idempotent).
UPDATE context_api_keys SET tenant_id = '00000000-0000-0000-0000-0000000d3fa0' WHERE tenant_id IS NULL;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (59, '059_tenants_hybrid.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 059_tenants_hybrid.sql

-- @@ ctx-fold begin 060_ctx_auth_tenant.sql
-- =============================================================================
-- 060_ctx_auth_tenant.sql — ctx_auth gains tenant identity + status gate +
--                           positional read_scopes (E1, Modell C)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Multi-Tenant wave T03/T15 (Achse 01 identity + Achse 02 visibility, fused into
-- ONE ctx_auth DROP+CREATE per the conflict-phase coordination, design/02 §9.1 /
-- design/05 §9 K1). Builds on 059 (context_api_keys.tenant_id/tenant_role +
-- context_tenants) and 061 (context_tenant_grants). Extends ctx_auth from 6 to 8
-- return columns — the 2 new columns (tenant_id UUID, tenant_role TEXT) are added
-- AT THE END so every named-column SELECT (auth.go:48) and every AuthResult{}
-- literal stays valid; old binaries that select the original 6 keep working.
--
-- DROP+CREATE, not CREATE OR REPLACE: adding a column changes the RETURNS TABLE
-- type, which OR REPLACE refuses ("cannot change return type"). Same mechanic as
-- 052:24-28. Migrations run before server start, so there is no live-traffic
-- window within the process.
--
-- THREE behavioural additions over the 052 body, each verified against the
-- canonical design (NOT the pre-Modell-C design/05 §3.4 sketch, which shows
-- tenant_id VARCHAR(50) + COALESCE(v_tenant_id, v_home_scope) — both Modell-A/B
-- relics that would crash under Modell C, see the valid-branch note):
--
--   1. Identity columns: the UPDATE...RETURNING also pulls tenant_id/tenant_role
--      (059). tenant_id is assigned DIRECTLY in the valid branch — it is already
--      a UUID; COALESCE(v_tenant_id, v_home_scope) would try 'work'::uuid and
--      crash. (design/01 §4.1; the §3.4 COALESCE is Modell-A/B only.)
--
--   2. Tenant status gate (design/01 §5.2), placed BEFORE the read_scopes build:
--      a lean single lookup on the tenant_id we already RETURNING'd (NOT the
--      §5.2 second key_hash JOIN — we hold v_tenant_id). The fail-closed
--      condition is `v_status IS NULL OR v_status <> 'active'`: a NULL
--      v_tenant_id, or a tenant row that vanished, leaves v_status NULL, and a
--      bare `<> 'active'` evaluates NULL (falsy) in plpgsql — fail-OPEN. The
--      explicit `IS NULL OR` arm closes that hole. Both 'suspended' and
--      'offboarding' fail the gate → __UNAUTHORIZED__ sentinel (is_valid=false),
--      reusing the same shape as a key-miss (no tenant-existence oracle, §5.1).
--      This is the differentiator of Modell C over B (B has no status row).
--
--   3. read_scopes built POSITIONALLY (design/02 §4.1 AMENDMENT, Variante A),
--      NOT via array_agg(DISTINCT). DISTINCT inside an aggregate forces an
--      alphabetical sort, which breaks the wire-load-bearing invariant
--      read_scopes[0] = home_scope (e.g. a 'work'-home key resolves {work,shared}
--      today; array_agg(DISTINCT) would flip it to {shared,work}, changing the
--      whoami wire (whoami.go:82) and the chat_sessions snapshot (chat.go:114)).
--      Element [1] is fixed to home_scope, then candidates (allowed, then grants
--      in a stable ORDER BY — that array_agg has an explicit ORDER BY, which is
--      deterministic, NOT a DISTINCT sort) are appended with an order-preserving
--      NOT-present dedup. The '_'-prefix filter (system scopes never visible) and
--      the COALESCE floor [home_scope] (never empty = never full access) are
--      unchanged in semantics from the 052 read_scopes line, only their
--      construction moves from concat to the positional loop.
--
-- MIGRATION ORDERING / plpgsql LATE-BINDING (the one empirically-load-bearing
-- point): the runner applies versions ASC (058,059,060,061; migrations.go:63),
-- so at THIS migration's CREATE time context_tenant_grants (version 61) does NOT
-- exist yet. plpgsql resolves table names in a function body at CALL time, not
-- CREATE time (check_function_bodies validates syntax, not table existence), so
-- CREATE FUNCTION succeeds against the not-yet-existing grants table. No call to
-- ctx_auth happens between 060 and 061 (the server starts only after ALL
-- migrations), so the grants table exists by first call. NO renumbering and NO
-- to_regclass guard are needed; the integration test's full 058→...→061 chain is
-- the empirical proof (if late-binding failed, SetupTestDB would error at 060).
--
-- lock_timeout (R-MIG2): DROP/CREATE FUNCTION takes only brief catalog locks (no
-- hot-table rewrite). The runner (internal/store/migrations.go) wraps EACH
-- migration in its own real transaction, so SET LOCAL is transaction-scoped and
-- self-reverting — it does not leak into the session or into 061+. We set it for
-- consistency with 058/059/061 and fail-fast hygiene (clean abort + re-runnable).
SET LOCAL lock_timeout = '3s';
--
-- Idempotent: DROP FUNCTION IF EXISTS + CREATE re-runs cleanly (second run drops
-- the just-created function and recreates it); _migrations INSERT ON CONFLICT
-- (version) DO NOTHING. Reversible: re-apply 052's ctx_auth body (forward-only in
-- practice). No data migration (function-only) — test.sh table count is UNCHANGED.
-- =============================================================================

DROP FUNCTION IF EXISTS ctx_auth(TEXT);

CREATE FUNCTION ctx_auth(p_api_key TEXT)
RETURNS TABLE (
    api_key_id     UUID,
    home_scope     VARCHAR(50),
    allowed_scopes TEXT[],
    read_scopes    TEXT[],
    is_valid       BOOLEAN,
    is_admin       BOOLEAN,
    tenant_id      UUID,    -- NEW (T03, Modell C): owning tenant; UUID, NOT VARCHAR
    tenant_role    TEXT     -- NEW (T03): owner|admin|member; plain TEXT (named Role type = T20)
) LANGUAGE plpgsql AS $$
DECLARE
    v_key_hash       TEXT;
    v_api_key_id     UUID;
    -- VARCHAR(50): PL/pgSQL enforces the typmod on assignment (052:58-60).
    v_home_scope     VARCHAR(50);
    v_allowed_scopes TEXT[];
    v_is_admin       BOOLEAN;
    v_tenant_id      UUID;        -- NEW
    v_tenant_role    TEXT;        -- NEW (TEXT, not VARCHAR — domain enforced by the 059 CHECK)
    v_status         TEXT;        -- NEW: tenant lifecycle status for the gate
    v_read_scopes    TEXT[];      -- NEW: positional build target (element [1] = home_scope)
    v_cand           TEXT[];      -- NEW: ordered candidate scopes (allowed, then grants)
    v_s              TEXT;        -- NEW: FOREACH cursor over candidates
BEGIN
    -- Compute SHA-256 hash of the provided API key.
    v_key_hash := encode(digest(p_api_key, 'sha256'), 'hex');

    -- Authenticate: update last_used_at and return key info, now including the
    -- Modell-C identity columns (059). last_used_at write-on-read is unchanged
    -- (R-SCALE7, the per-request write storm at N tenants, is a later seam).
    UPDATE context_api_keys
    SET last_used_at = now()
    WHERE context_api_keys.key_hash = v_key_hash
      AND context_api_keys.active = true
    RETURNING
        context_api_keys.id,
        context_api_keys.home_scope,
        context_api_keys.allowed_scopes,
        context_api_keys.is_admin,
        context_api_keys.tenant_id,
        context_api_keys.tenant_role
    INTO v_api_key_id, v_home_scope, v_allowed_scopes, v_is_admin, v_tenant_id, v_tenant_role;

    -- Key miss: sentinel (unchanged shape; +2 explicit new columns).
    -- TENANT-DECISION(sentinel-tenant-role): '' (leer) — Alt 'member',
    --   umentscheidbar weil is_valid=false ohnehin in middleware.go vor jedem
    --   Handler stoppt; '' folgt design/01 §5.2:561 (Sentinel-Konsistenz).
    IF v_api_key_id IS NULL THEN
        api_key_id     := NULL;
        home_scope     := '__UNAUTHORIZED__';
        allowed_scopes := '{}'::TEXT[];
        read_scopes    := '{}'::TEXT[];
        is_valid       := false;
        is_admin       := false;
        tenant_id      := NULL;
        tenant_role    := '';
        RETURN NEXT;
        RETURN;
    END IF;

    -- Tenant status gate (design/01 §5.2), BEFORE the read_scopes build. Lean
    -- single lookup on the tenant_id we already hold. `IS NULL OR <> 'active'`
    -- is the fail-closed condition (a bare `<> 'active'` would be NULL/falsy =
    -- fail-OPEN when v_tenant_id is NULL or the tenant row is gone). suspended
    -- AND offboarding both fail → same __UNAUTHORIZED__ sentinel as a key-miss.
    SELECT status INTO v_status FROM context_tenants WHERE id = v_tenant_id;
    IF v_status IS NULL OR v_status <> 'active' THEN
        api_key_id     := NULL;
        home_scope     := '__UNAUTHORIZED__';
        allowed_scopes := '{}'::TEXT[];
        read_scopes    := '{}'::TEXT[];
        is_valid       := false;
        is_admin       := false;
        tenant_id      := NULL;
        tenant_role    := '';
        RETURN NEXT;
        RETURN;
    END IF;

    -- read_scopes POSITIONAL (design/02 §4.1 amendment, Variante A). Element [1]
    -- is home_scope (the wire-load-bearing read_scopes[0] = home invariant); then
    -- candidates (allowed, then cross-tenant grants for THIS key's tenant) are
    -- appended order-preserving with a NOT-present dedup. array_agg(DISTINCT) is
    -- FORBIDDEN (alphabetical sort breaks the invariant). The grant array_agg
    -- uses an explicit ORDER BY (deterministic, NOT a DISTINCT sort).
    -- TENANT-DECISION(read-scopes-variant): A (FOREACH-Append) — Alt B (unnest
    --   WITH ORDINALITY), umentscheidbar weil äquivalent; A ist am direktesten
    --   les- und im Mutationstest pinbar (design/02 §4.1).
    v_read_scopes := ARRAY[v_home_scope::TEXT];
    v_cand := COALESCE(v_allowed_scopes, '{}'::TEXT[])
           || COALESCE((SELECT array_agg(g.granted_scope ORDER BY g.granted_scope)
                          FROM context_tenant_grants g
                         WHERE g.grantee_tenant = v_tenant_id), '{}'::TEXT[]);
    FOREACH v_s IN ARRAY v_cand LOOP
        -- fail-closed: '_'-prefixed system scopes never enter read_scopes
        -- (backslash-escaped LIKE; '\' is the default LIKE escape char). home_scope
        -- itself is NOT filtered — it is the MINIMAL view even if '_'-prefixed.
        IF v_s NOT LIKE '\_%' AND NOT (v_s = ANY(v_read_scopes)) THEN
            v_read_scopes := v_read_scopes || v_s;
        END IF;
    END LOOP;

    -- Valid key (+2 new columns). tenant_id assigned DIRECTLY (already a UUID;
    -- COALESCE(v_tenant_id, v_home_scope) — the design/05 §3.4 Modell-A/B sketch —
    -- would attempt 'work'::uuid and crash). The read_scopes COALESCE floor keeps
    -- [home_scope] for the degenerate '_'-home case (minimal view, never empty).
    api_key_id     := v_api_key_id;
    home_scope     := v_home_scope;
    allowed_scopes := v_allowed_scopes;
    read_scopes    := COALESCE(NULLIF(v_read_scopes, '{}'::TEXT[]), ARRAY[v_home_scope]::TEXT[]);
    is_valid       := true;
    is_admin       := v_is_admin;
    tenant_id      := v_tenant_id;
    tenant_role    := v_tenant_role;
    RETURN NEXT;
    RETURN;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (60, '060_ctx_auth_tenant.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 060_ctx_auth_tenant.sql

-- @@ ctx-fold begin 061_tenant_grants.sql
-- =============================================================================
-- 061_tenant_grants.sql — Cross-tenant READ channel (E1, Modell C)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Multi-Tenant wave T14 (Achse 02-V1, the genuine schema contribution of the
-- visibility axis). Builds on 059 (context_tenants + context_tenant_scopes +
-- context_api_keys.tenant_id/tenant_role). Exact schema vorlage:
-- design/02-scope-visibility.md §V1 (Z.131-141).
--
-- context_tenant_grants is the cross-tenant READ channel: one tenant grants
-- ANOTHER tenant read access to one of its OWN scopes (the "friend-tenant" case,
-- design/02 §3.1/§3.3). ADDITIVE table with NO consumer yet — the read_scopes
-- resolver body in migration 060 (a LATER wave) will UNION it into ctx_auth;
-- 061 only lays the table. No backfill (empty initially: cross-tenant sharing is
-- opt-in, least-privilege).
--
-- SEMANTIK (FK + uq):
--   grantee_tenant FK → context_tenants(id) ON DELETE CASCADE
--     — the tenant RECEIVING the read access. Tenant gone → its held grants go.
--   granted_scope  FK → context_tenant_scopes(scope) ON DELETE CASCADE
--     — the scope being shared. Scope removed at offboarding → grant gone. This
--       FK is the FAIL-CLOSED-BY-CONSTRUCTION guard: a grant can ONLY target a
--       registered, tenant-owned scope. System scopes ('_global', '_'-prefixed)
--       are NEVER in context_tenant_scopes (059 maps only private/work/shared;
--       the '_'-prefix is double-enforced on key creation), so a grant on them
--       is rejected by the FK automatically (23503) — NO dedicated _-prefix
--       CHECK is needed on this table.
--   created_by     FK → context_api_keys(id) ON DELETE SET NULL
--     — provenance: which key issued the grant. Key delete ANONYMIZES the
--       history (NULLs created_by), it NEVER deletes the grant (the share
--       survives the key that minted it).
--   uq_tenant_grant (grantee_tenant, granted_scope)
--     — idempotent double-grant guard: re-granting the same (tenant, scope) pair
--       is a no-op at the schema level (23505), never a duplicate row.
--
-- lock_timeout (R-MIG2): this migration only CREATEs new objects (no ALTER on a
-- hot table), so it takes no long-held lock on existing data. The Runner
-- (internal/store/migrations.go) wraps EACH migration in its own real
-- transaction, so SET LOCAL is transaction-scoped and self-reverting — it does
-- not leak into the session or into 062+. We still set it for consistency with
-- 058/059 and fail-fast hygiene (clean abort + re-runnable) rather than
-- unbounded lock-waiting.
SET LOCAL lock_timeout = '3s';
--
-- Idempotent: CREATE TABLE IF NOT EXISTS / CREATE INDEX IF NOT EXISTS /
-- _migrations INSERT ON CONFLICT (version) DO NOTHING. No backfill to be no-op
-- about (the table starts empty). Reversible: DROP TABLE context_tenant_grants
-- (forward-only in practice).
--
-- Note: 060 (the read_scopes resolver plpgsql function that CONSUMES this table)
-- is a LATER wave and is intentionally absent here. The Runner applies
-- migrations per-version (EXISTS check, migrations.go:69-79), so an out-of-order
-- gap (061 present, 060 missing) is tolerated; 060 arrives later and references
-- this table.
-- =============================================================================

-- context_tenant_grants — cross-tenant read grants (grantee gets read on a scope
-- owned by another tenant). uuidv7() is PG18-native (001_initial.sql:58, also
-- used in 059).
CREATE TABLE IF NOT EXISTS context_tenant_grants (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    grantee_tenant  UUID NOT NULL REFERENCES context_tenants(id)              ON DELETE CASCADE,
    granted_scope   VARCHAR(50) NOT NULL REFERENCES context_tenant_scopes(scope) ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by      UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,
    metadata        JSONB NOT NULL DEFAULT '{}',
    CONSTRAINT uq_tenant_grant UNIQUE (grantee_tenant, granted_scope)
);

-- Lookup path for the resolver (060): "which scopes is THIS tenant granted?"
CREATE INDEX IF NOT EXISTS idx_tenant_grants_grantee ON context_tenant_grants (grantee_tenant);

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (61, '061_tenant_grants.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 061_tenant_grants.sql

-- @@ ctx-fold begin 062_backend_pool_tenant.sql
-- =============================================================================
-- 062_backend_pool_tenant.sql — context_backends scope dimension (MT, Achse 04)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Multi-Tenant wave T33a (Achse 04-W1, per-tenant backend pool). Builds on 053
-- (context_backends + audit trigger). Exact schema vorlage: design/04 §3.1.
--
-- context_backends gains a scope dimension: '_global' = shared server backend
-- (today's 5 rows), '<tenant>' = tenant-private (a friend-tenant's own key).
-- '_global' is consistent with the context_settings/secrets convention (051:42):
-- the '_' prefix is system-reserved (firstReservedScope, api_keys.go:44), so no
-- tenant home_scope can ever collide with it.
--
-- BEHAVIORALLY NEUTRAL: the 5 live backends become '_global' through the
-- DEFAULT (PG18 metadata-only ADD COLUMN, no per-row UPDATE), and Chain() does
-- not yet filter on scope (04-W2). The read path (loadBackendsSQL/scanBackend +
-- Backend.Scope) loads it additively; the write path stays scope-default until
-- per-tenant-admin (04-W5). Forward-compatible with the old binary: a SELECT
-- with a fixed column list ignores the new column, so 062 may run before deploy.
--
-- UNIQUE swap (Befund C, db-tenant-surface.md §8): uq_backends_name (name) →
-- uq_backends_scope_name (scope, name), so two tenants can each own a backend
-- 'openrouter' without colliding with the shared '_global' one. Collision-free
-- on the 5 live rows: all become '_global', and the old UNIQUE(name) and the new
-- UNIQUE(scope,name) are coincident on that set.
--
-- Idempotent: ADD COLUMN / DROP CONSTRAINT / CREATE INDEX are IF [NOT] EXISTS;
-- the unguarded ADD CONSTRAINT is safe because the runner skips an already-
-- applied version (per-version EXISTS check) and a failed file rolls back whole
-- (single Tx) — a re-run drops uq_backends_name then adds the new one once.
-- Out-of-order gap (063 quota is a later sub-wave; 064 settings already exists
-- from Achse 03) is tolerated by the runner. Forward-only, self-registering.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE context_backends
    ADD COLUMN IF NOT EXISTS scope VARCHAR(50) NOT NULL DEFAULT '_global';

-- Backfill is already done by the DEFAULT; explicit for clarity / re-run.
UPDATE context_backends SET scope = '_global' WHERE scope IS NULL;

-- UNIQUE swap: name → (scope, name).
ALTER TABLE context_backends DROP CONSTRAINT IF EXISTS uq_backends_name;
ALTER TABLE context_backends
    ADD CONSTRAINT uq_backends_scope_name UNIQUE (scope, name);

-- Per-tenant browse path (loadBackendsSQL WHERE scope = ANY($1) in 04-W2); scope
-- leads, enabled included because Chain filters enabled.
CREATE INDEX IF NOT EXISTS idx_backends_scope
    ON context_backends (scope, enabled);

-- Audit-trigger adjustment: audit_backends_write (053:71-116) hardcodes
-- scope='_global' in the audit row (053:99). Record the backend's own scope
-- instead — the exact pattern audit_settings_write uses (051:126) — so a
-- tenant-private backend mutation is tenant-attributed automatically. CREATE OR
-- REPLACE (no DROP, no signature change); the existing trigger picks up the new
-- body by name.
CREATE OR REPLACE FUNCTION audit_backends_write() RETURNS TRIGGER AS $$
DECLARE
    v_new   JSONB := to_jsonb(NEW);
    v_old   JSONB := to_jsonb(OLD);
    v_actor UUID  := NULLIF(current_setting('ctx.api_key_id', true), '')::uuid;
    v_label TEXT;
BEGIN
    IF v_new ? 'extra_headers' THEN
        SELECT jsonb_set(v_new, '{extra_headers}',
                         COALESCE(jsonb_object_agg(k, '"[redacted]"'::jsonb), '{}'::jsonb))
          INTO v_new
          FROM jsonb_object_keys(v_new->'extra_headers') AS k;
    END IF;
    IF v_old ? 'extra_headers' THEN
        SELECT jsonb_set(v_old, '{extra_headers}',
                         COALESCE(jsonb_object_agg(k, '"[redacted]"'::jsonb), '{}'::jsonb))
          INTO v_old
          FROM jsonb_object_keys(v_old->'extra_headers') AS k;
    END IF;
    IF v_actor IS NOT NULL THEN
        SELECT label INTO v_label FROM context_api_keys WHERE id = v_actor;
    END IF;
    INSERT INTO context_settings_audit
        (entity_type, entity_key, scope, action, old_value, new_value,
         api_key_id, actor_label, metadata)
    VALUES (
        'backend',
        COALESCE(v_new->>'name', v_old->>'name'),
        COALESCE(v_new->>'scope', v_old->>'scope'),
        CASE TG_OP WHEN 'INSERT' THEN 'create'
                   WHEN 'UPDATE' THEN 'update'
                   ELSE 'delete' END,
        v_old, v_new,
        v_actor, v_label,
        jsonb_strip_nulls(jsonb_build_object(
            'via',        CASE WHEN v_actor IS NULL THEN 'sql' ELSE 'api' END,
            'request_id', NULLIF(current_setting('ctx.request_id', true), '')))
    );
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (62, '062_backend_pool_tenant.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 062_backend_pool_tenant.sql

-- @@ ctx-fold begin 063_tenant_quota.sql
-- =============================================================================
-- 063_tenant_quota.sql — per-tenant cost/call budgets + accounting indices (MT, Achse 04)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Multi-Tenant wave T33b (Achse 04-W1, second half). Exact schema vorlage:
-- design/04 §3.2. Quota ENFORCEMENT does not exist today (cost_usd 054:22 +
-- api_key_id 054:23 are logged, never checked) — this lays the policy table
-- (mechanism = code in the enforcement wave 04-W4; policy = this data) plus the
-- accounting / rate-limit access paths the cost-attribution (04-W3) and quota
-- (04-W4) waves read.
--
-- BEHAVIORALLY NEUTRAL / pausable: nothing reads the table yet, and a missing
-- quota row means "no limit" — fail-OPEN by design (a missing row meaning "0"
-- would lock every tenant out the moment the table is created; the fail-CLOSED
-- axis is egress visibility 04-W2, not the cost budget). No CHECK on scope
-- (v2.0.0 line, like 051/053): scope is the free tenant axis; an unknown scope
-- is a new tenant, not a broken gate.
--
-- The NOTIFY trigger rides the same channel as 051/053 (notify_settings_write,
-- 051:91) so a quota write hot-reloads the per-tenant snapshot rather than a
-- hot-path SELECT. Until 065 (Achse 03) carries scope in the NOTIFY payload, a
-- quota write invalidates globally — correct, just less efficient (K-N1); the
-- trigger inherits the scope payload automatically once 065 lands.
--
-- Index Tx trap (db-tenant-surface.md §1, N10): the runner holds each migration
-- in ONE Tx, so CREATE INDEX CONCURRENTLY is forbidden. On context_llm_log
-- (hypertable) the index is built per chunk; on context_access_log (heap, ~91k
-- live rows) a non-concurrent CREATE INDEX is a blocking build on one relation —
-- at large existing volume run it OUT OF BAND (manual CREATE INDEX CONCURRENTLY
-- before the migration deploy; the migration then finds it via IF NOT EXISTS).
-- A tooling seam (N10), not a schema question.
--
-- Idempotent (CREATE TABLE/INDEX IF NOT EXISTS, DROP TRIGGER IF EXISTS + CREATE,
-- _migrations ON CONFLICT). Forward-only, self-registering. 063 closes the
-- 062→064 numbering gap (058-064 now contiguous).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_tenant_quota (
    id                  UUID PRIMARY KEY DEFAULT uuidv7(),
    scope               VARCHAR(50) NOT NULL,        -- the tenant scope (NOT '_global'-capable:
                                                     -- the global default quota lives as a settings key)
    -- Window budgets; NULL = unlimited (fail-OPEN: a missing budget is "no limit").
    daily_cost_usd      NUMERIC(14,8),               -- max external cost / 24h rolling window
    monthly_cost_usd    NUMERIC(14,8),               -- max external cost / 30d rolling window
    daily_calls         INTEGER,                     -- max ATTRIBUTED wire calls / 24h (api_key_id-carried;
                                                     -- does NOT fold background/dream — they carry no api_key_id)
    -- Action on COST-budget exceed: 'block' (Chain returns ErrQuotaExceeded,
    -- external too; local stays) or 'external_off' (only external backends drop
    -- from the chain, local stays). Default 'external_off' = degrade, not lock.
    on_exceed           TEXT NOT NULL DEFAULT 'external_off'
                        CHECK (on_exceed IN ('block','external_off')),
    enabled             BOOLEAN NOT NULL DEFAULT true,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by          UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,
    metadata            JSONB NOT NULL DEFAULT '{}',
    CONSTRAINT uq_tenant_quota_scope UNIQUE (scope)  -- one quota row per tenant
);

-- Hot-reload of the quota policy on the settings NOTIFY channel.
DROP TRIGGER IF EXISTS trg_tenant_quota_notify ON context_tenant_quota;
CREATE TRIGGER trg_tenant_quota_notify
    AFTER INSERT OR UPDATE OR DELETE ON context_tenant_quota
    FOR EACH ROW EXECUTE FUNCTION notify_settings_write();

-- Per-tenant cost-rollup access path: the SUM reads WHERE api_key_id = ANY($keys)
-- AND created_at > now() - window (the keys resolved to a literal UUID list, §6.4).
CREATE INDEX IF NOT EXISTS idx_llm_log_apikey
    ON context_llm_log (api_key_id, created_at DESC) WHERE api_key_id IS NOT NULL;

-- Egress-cost rollup: external + time, cost_usd COVERING (INCLUDE) so the 24h
-- SUM is index-only (no per-row heap fetch). Partial on external keeps it narrow.
CREATE INDEX IF NOT EXISTS idx_llm_log_cost
    ON context_llm_log (created_at DESC) INCLUDE (cost_usd)
    WHERE backend_locality = 'external' AND cost_usd IS NOT NULL;

-- Rate-limit access path: the per-key 60s count (CheckRateLimitByAction,
-- store/blocks.go:277-285) filters api_key_id+action; this composite makes it
-- tenant-selective (today it scans all tenants via the time-only index).
CREATE INDEX IF NOT EXISTS idx_access_log_ratelimit
    ON context_access_log (api_key_id, action, created_at DESC);

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (63, '063_tenant_quota.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 063_tenant_quota.sql

-- @@ ctx-fold begin 064_settings_tenant_index.sql
-- =============================================================================
-- 064_settings_tenant_index.sql — scope-leading read indexes (MT, Achse 03)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Multi-Tenant wave T27 (Achse 03-W1, settings/secrets tenant resolution).
-- Builds on 051 (context_settings / context_secrets, both with a scope column).
-- Exact schema vorlage: design/03-settings-secrets-mt.md §3.2.
--
-- The per-tenant resolution reads WHERE scope = ANY({tenant, '_global'}) and
-- orders by key/name. The existing uq_settings_key_scope (key, scope) and
-- uq_secrets_name_scope (name, scope) are key/name-LEADING, so they don't serve
-- a scope-first filter. A scope-LEADING index serves that access pattern
-- directly and keeps the per-tenant read at one tenant's row count, not a full
-- table scan as the corpus grows to N tenants (target scale: 1M blocks x N
-- tenants; settings rows stay few PER tenant but the table is shared).
--
-- ADDITIVE, no consumer yet: store.LoadSettingOverridesMulti (this wave) is the
-- only reader and has no call site until the resolution waves (03-W3+). Pure
-- read-path optimization — no structural change (scope is already VARCHAR(50),
-- already in the UNIQUE keys, already mirrored by the audit trigger).
-- Idempotent, forward-only, self-registering (M031+ convention, 057:77-78).
--
-- Tx note (058/059/061 convention): lock_timeout is Tx-scoped (SET LOCAL).
-- CREATE INDEX CONCURRENTLY is forbidden inside the runner's single-Tx-per-file
-- (migrations.go), but context_settings/context_secrets each carry ~1 row today
-- and stay few-per-tenant, so a non-concurrent CREATE INDEX is lock-trivial.
-- Out-of-order gap (062/063 belong to Achse 04, built later) is tolerated by the
-- runner's per-version EXISTS check (cf. 061 header).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE INDEX IF NOT EXISTS idx_settings_scope_key
    ON context_settings (scope, key);

CREATE INDEX IF NOT EXISTS idx_secrets_scope_name
    ON context_secrets (scope, name);

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (64, '064_settings_tenant_index.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 064_settings_tenant_index.sql

-- @@ ctx-fold begin 065_settings_notify_scope.sql
-- =============================================================================
-- 065_settings_notify_scope.sql — scope in the settings NOTIFY payload (MT, Achse 03)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Multi-Tenant wave T32 (Achse 03-W6, scope-carried lazy cache invalidation).
-- Builds on 051 (notify_settings_write + the ctx_settings_write channel). Exact
-- spec: design/03-settings-secrets-mt.md §MT3-W6 / §6.3; conflict K-N1.
--
-- The 051 payload carried {entity, key, op}. Per-tenant config snapshots
-- (Achse 06) need to know WHICH scope changed so the NOTIFY listener can drop
-- exactly the affected tenant's cached config generation instead of rebuilding
-- every tenant's generation on every write (the pre-W6 fallback: every write
-- invalidates all — correct, just O(N) lazy rebuilds). This adds the row's
-- scope to the payload, ADDITIVELY: an old listener ignores the extra field, a
-- new one (events/listener.go, T32) routes a tenant scope → InvalidateTenant,
-- a _global / reserved / absent scope → full Reload (base rebuild + cache wipe).
--
-- to_jsonb(COALESCE(NEW, OLD)) already exposes the scope column for ALL four
-- tables that fire this trigger — context_settings (051), context_secrets
-- (051), context_backends (053) and context_tenant_quota (063) each carry a
-- scope column — so v_row->>'scope' resolves on every firing path; the 063
-- quota trigger inherits the scope payload automatically (it EXECUTEs the same
-- function). A hypothetical scope-less table would yield SQL NULL → JSON null →
-- Go "" → the safe _global/full-reload branch (fail-safe over-invalidation).
--
-- CREATE OR REPLACE updates the function body in place; every trigger that
-- EXECUTEs it picks up the new body on its next firing — no trigger re-create
-- needed. Function-only, no table/column change: test.sh table count unchanged
-- (cf. 060). Idempotent (CREATE OR REPLACE + ON CONFLICT DO NOTHING), forward-
-- only, self-registering (M031+ convention). Out-of-order gap (067 already
-- present from T39) is tolerated by the runner's per-version EXISTS check.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE OR REPLACE FUNCTION notify_settings_write() RETURNS TRIGGER AS $$
DECLARE
    v_row JSONB := to_jsonb(COALESCE(NEW, OLD));
BEGIN
    PERFORM pg_notify('ctx_settings_write', json_build_object(
        'entity', TG_TABLE_NAME,
        'key',    COALESCE(v_row->>'key', v_row->>'name'),
        'scope',  v_row->>'scope',
        'op',     TG_OP)::text);
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (65, '065_settings_notify_scope.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 065_settings_notify_scope.sql

-- @@ ctx-fold begin 067_block_grants.sql
-- =============================================================================
-- 067_block_grants.sql — row-level read grant for single blocks (MT, Achse 07)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Multi-Tenant wave T39 (Achse 07-W1). The THIRD level of the 3-level
-- architecture (user 2026-06-15): after tenant (059) and scope/department, the
-- single-block share. "einzelne bloecke an andere freigeben". Exact schema
-- vorlage: design/07-block-level-sharing.md §3.1.
--
-- A block STAYS in its scope; this grant makes it additively READABLE for one
-- grantee tenant — read-only, revocable, immediate. FINER than
-- context_tenant_grants (061, scope-level):
--   scope-level: (grantee_tenant, granted_scope)   -- 061
--   block-level: (grantee_tenant, block_id)        -- THIS table
--
-- Mechanism = code (the OR-arm in the switch point), policy = data (THIS table).
-- ADDITIVE table with NO consumer yet — the VisibilityPredicate OR-arm (T40a)
-- and the ctx_rrf sixfold OR (T40b/068) read it LATER. 067 only lays the table.
-- With an empty grant set the whole mechanism is a byte-identical no-op to the
-- scope-only state (pausability invariant, conflicts.md §7).
--
-- Backfill-free (0 existing grants — block-level sharing is opt-in,
-- least-privilege). NEW table + index on an EMPTY relation → no context_blocks
-- lock, no 1M index build, no CONCURRENTLY problem (the decisive advantage of
-- the join over the array variant, inventory/rowlevel-acl-scale.md §5.3 /
-- design/07 §3.2). Idempotent (CREATE ... IF NOT EXISTS, _migrations ON
-- CONFLICT), self-registering, forward-only, one Tx.
--
-- ON DELETE policies:
--   block_id        FK → context_blocks(id)   ON DELETE CASCADE
--     — a deleted block cannot hold a grant; the grant vanishes with it.
--       KONTRAST to context_dream_links (no ON DELETE → blocks the delete);
--       here the cleaner offboarding wins (design/07 §3.1).
--   grantee_tenant  FK → context_tenants(id)  ON DELETE CASCADE
--     — grantee offboarding clears the grants it HOLDS (design/01 §6.3).
--   granted_by      FK → context_api_keys(id) ON DELETE SET NULL
--     — provenance pointer; key delete ANONYMIZES it, never deletes the grant
--       (the share survives the key that minted it; FK politik db-tenant-surface §8).
--
-- lock_timeout (R-MIG2): CREATE-only, no ALTER on a hot table → no long-held
-- lock. The Runner (internal/store/migrations.go) wraps EACH migration in its
-- own Tx, so SET LOCAL is transaction-scoped and self-reverting. Set for
-- consistency with 058-063 and fail-fast hygiene.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_block_grants (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    block_id        UUID NOT NULL REFERENCES context_blocks(id)  ON DELETE CASCADE, -- the shared block
    grantee_tenant  UUID NOT NULL REFERENCES context_tenants(id) ON DELETE CASCADE, -- gains read access (Modell C, UUID-FK)
    granted_by      UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,        -- audit pointer (FK politik db-tenant-surface §8)
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata        JSONB NOT NULL DEFAULT '{}',
    CONSTRAINT uq_block_grant UNIQUE (block_id, grantee_tenant)   -- no double-grant; also covers block_id-leading lookup
);
-- TENANT-DECISION(block-grant-grantee): grantee_tenant (UUID FK on context_tenants) —
--   Alternative grantee_scope (department) OR grantee_key (single key), umentscheidbar
--   weil die Spalte additiv erweiterbar ist (mehrere grantee_*-Spalten mit XOR-CHECK)
--   und der Switch-Point-OR nur die aufgeloeste id-Liste sieht. Tenant-Granularitaet
--   ist der minimale Schnitt (analog scope-level grantee_tenant, 061); feiner (scope/key)
--   ist additiv nachruestbar. design/07 §3.1 / §8, inventory rowlevel-acl-scale ED-B2.
-- TENANT-DECISION(block-grant-permission): KEINE permission-Spalte in 067 — read-only by
--   construction (the write path gates on home_scope, design/07 §2.5). Alternative
--   permission TEXT DEFAULT 'read' CHECK (permission IN ('read')) JETZT, umentscheidbar
--   weil die Tabelle KLEIN ist → ein spaeteres `ADD COLUMN permission DEFAULT 'read'` bei
--   der ersten Schreib-/Comment-Grant-Welle (Achse 05) ist lock-arm. Eine Single-Value-
--   CHECK-Enum gatet HEUTE nichts (jede Zeile traegt zwangslaeufig 'read') → minimaler
--   Schnitt (M052-is_admin-Doktrin) spricht GEGEN sie jetzt. Wenn 'write'-Grants kommen,
--   MUSS der Lese-OR dann `AND permission IN ('read',...)` filtern. design/07 §3.1.

-- Two lookup directions (analog context_dream_links 016: PK + idx_target):
--   (a) "which blocks are granted to tenant A?" → grantee-leading (HOT read, the resolver)
CREATE INDEX IF NOT EXISTS idx_block_grants_grantee
    ON context_block_grants (grantee_tenant, block_id);
--   (b) "who sees this block?" / revoke / audit → block_id-leading, covered by uq_block_grant

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (67, '067_block_grants.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 067_block_grants.sql

-- @@ ctx-fold begin 068_rrf_block_grants.sql
-- =============================================================================
-- 068_rrf_block_grants.sql — ctx_rrf block-level read grant OR-arm (MT, Achse 07)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Multi-Tenant wave T40b (Achse 07-W5, the expensive RRF-retrieval OR-arm).
-- Builds on 067 (context_block_grants) + T40a (the cheap abruf/ID paths +
-- VisibilityPredicate grant arm). Exact spec: design/07-block-level-sharing.md
-- §2.2/§2.3/§4.2/§5.3.2 + Welle T40b.
--
-- T40a wired the row-level grant OR-arm into every CHEAP read path (GetBlock,
-- ResolveBlockID, SearchBlocks, EgoGraph, the 3 MCP handlers) via the shared
-- store.VisibilityPredicate. ctx_rrf does NOT reference that Go fragment — it
-- carries the same visibility triple SIX times inline (one WHERE per RRF
-- channel: semantic / fulltext_de / fulltext_en / trigram_title / block_mass /
-- block_role_factor). This migration is the SECOND materialisation of the OR-arm
-- (the SQL side), so a granted block surfaces in semantic retrieval too — not
-- only via direct abruf.
--
-- Mechanism: a 13th parameter p_granted_block_ids UUID[] DEFAULT NULL (M048
-- DROP+CREATE schablone, 048:31-32/45-46), and at each of the SIX CTE WHERE
-- clauses the flat term `AND cb.scope = ANY(p_scopes)` becomes an EXPLICITLY
-- PARENTHESISED disjunction:
--   AND ( cb.scope = ANY(p_scopes)
--         OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
--
-- CRITICAL (design/07 §4.2/§5.3.2, operator precedence): ctx_rrf carries NO
-- parentheses around the scope term today (a flat AND chain, 048:69-77) — the
-- inner (scope OR id) parens must be CREATED. SQL binds AND tighter than OR.
-- WITHOUT the inner parens the OR binds at top level and the preceding
-- `NOT is_archived` / `block_role <> 'system-meta'` conjuncts are bypassed for
-- the grant arm → a granted ARCHIVED or system-meta block would leak. The
-- archived/system-meta conjuncts therefore stand strictly BEFORE the parens.
-- The `p_granted_block_ids IS NOT NULL` guard makes the OR a deterministic no-op
-- for every legacy caller (NULL = no block-level), exactly like the
-- `p_categories_exclude IS NULL` branch — so existing callers are byte-identical.
--
-- Return-Type UNCHANGED → backward-compatible additive. Function-only, no
-- table/column change: test.sh table count unchanged (cf. 060/065). Idempotent
-- (DROP IF EXISTS + CREATE OR REPLACE + ON CONFLICT DO NOTHING), forward-only,
-- self-registering (M031+ convention). Out-of-order gap (066 absent) tolerated
-- by the runner's per-version EXISTS check.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- Drop the current 12-param signature (048) and the new 13-param signature (for
-- idempotent re-runs) — M048 pattern (048:31-32). CREATE OR REPLACE alone would
-- leave the old 12-arg overload alive alongside the new one.
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT, DOUBLE PRECISION, TEXT[], TEXT[]);
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT, DOUBLE PRECISION, TEXT[], TEXT[], UUID[]);

CREATE OR REPLACE FUNCTION ctx_rrf(
    p_embedding             halfvec(1024),
    p_query                 TEXT,
    p_query_spaced          TEXT,
    p_scopes                TEXT[],
    p_category              TEXT DEFAULT NULL,
    p_tags                  TEXT[] DEFAULT NULL,
    p_limit                 INT DEFAULT 5,
    p_temporal              TEXT DEFAULT NULL,
    p_query_or              TEXT DEFAULT NULL,
    p_audit_trail_factor    DOUBLE PRECISION DEFAULT 1.0,
    p_categories_exclude    TEXT[] DEFAULT NULL,
    p_block_roles_exclude   TEXT[] DEFAULT NULL,
    p_granted_block_ids     UUID[] DEFAULT NULL
) RETURNS TABLE (
    rrf_score   DOUBLE PRECISION,
    cosine_sim  DOUBLE PRECISION,
    id          UUID,
    title       TEXT,
    category    VARCHAR(100),
    tags        TEXT[],
    content     TEXT,
    scope       VARCHAR(50),
    updated_at  TIMESTAMPTZ
) LANGUAGE plpgsql AS $$
BEGIN
    SET LOCAL hnsw.iterative_scan = 'relaxed_order';

    RETURN QUERY
    WITH semantic AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
            ) AS rank,
            1.0 - (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS cos_sim
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.block_role IS NULL OR cb.block_role != ALL(p_block_roles_exclude))
        ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
        LIMIT 75
    ),
    fulltext_de AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query)),
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.block_role IS NULL OR cb.block_role != ALL(p_block_roles_exclude))
          AND (
              cb.ts_de @@ plainto_tsquery('german', p_query)
              OR cb.ts_de @@ plainto_tsquery('german', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    fulltext_en AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.block_role IS NULL OR cb.block_role != ALL(p_block_roles_exclude))
          AND (
              cb.ts_en @@ plainto_tsquery('english', p_query)
              OR cb.ts_en @@ plainto_tsquery('english', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    trigram_title AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY similarity(cb.title, p_query) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.block_role IS NULL OR cb.block_role != ALL(p_block_roles_exclude))
          AND similarity(cb.title, p_query) > 0.05
        LIMIT 30
    ),
    block_mass AS (
        SELECT cb.id,
               CASE
                   WHEN array_length(cb.content_times, 1) IS NULL THEN 1.0
                   WHEN array_length(cb.content_times, 1) = 0    THEN 1.0
                   ELSE 1.0 / sqrt(array_length(cb.content_times, 1)::DOUBLE PRECISION)
               END AS mass_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.block_role IS NULL OR cb.block_role != ALL(p_block_roles_exclude))
    ),
    block_role_factor AS (
        SELECT cb.id,
               CASE
                   WHEN cb.block_role = 'audit-trail' THEN p_audit_trail_factor
                   ELSE 1.0
               END AS role_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.block_role IS NULL OR cb.block_role != ALL(p_block_roles_exclude))
    ),
    rrf AS (
        SELECT
            COALESCE(s.id, d.id, e.id, g.id) AS block_id,
            (   0.45 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + s.rank), 0)
              + 0.20 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + d.rank), 0)
              + 0.25 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + e.rank), 0)
              + 0.10 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + g.rank), 0)
            )::DOUBLE PRECISION AS score,
            s.cos_sim::DOUBLE PRECISION
        FROM semantic s
        FULL OUTER JOIN fulltext_de d USING (id)
        FULL OUTER JOIN fulltext_en e USING (id)
        FULL OUTER JOIN trigram_title g USING (id)
        LEFT JOIN block_mass m ON m.id = COALESCE(s.id, d.id, e.id, g.id)
        LEFT JOIN block_role_factor rf ON rf.id = COALESCE(s.id, d.id, e.id, g.id)
    )
    SELECT
        r.score                    AS rrf_score,
        r.cos_sim                  AS cosine_sim,
        cb.id,
        cb.title::TEXT             AS title,
        cb.category,
        cb.tags,
        cb.content,
        cb.scope::VARCHAR(50)      AS scope,
        cb.updated_at
    FROM rrf r
    JOIN context_blocks cb ON cb.id = r.block_id
    ORDER BY r.score DESC
    LIMIT p_limit;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (68, '068_rrf_block_grants.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 068_rrf_block_grants.sql

-- @@ ctx-fold begin 069_tenant_limits.sql
-- =============================================================================
-- 069_tenant_limits.sql — strukturelle per-Tenant-Limits (max_scopes/max_keys)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Self-Service-Onboarding-Achse (BE5/BEQ). Trägt die ZWEI strukturellen
-- Tenant-Limits, gegen die die Self-Service-Anlage von Scopes (scope-create) und
-- Keys (api-key-create) transaktional deckelt (AcquireTenantSlot, BEQ-2). KLAR
-- abgegrenzt von 063_tenant_quota (context_tenant_quota = LLM-KOSTEN-Budget,
-- scope-keyed): das hier sind strukturelle ZÄHL-Limits auf dem Tenant, kein Geld.
--
-- WARUM typisierte Spalten statt metadata-JSONB: ein Security-Control braucht eine
-- DB-validierte Schranke (CHECK >= 0), nicht eine durch jeden anderen metadata-
-- Writer clobberbare, ungeprüfte JSONB-Zelle. Eine additive NULL-fähige Spalte mit
-- Default ist der normale Migrations-Lauf alt→neu (kein BREAKING, kein Backfill).
--
-- DEFAULT-SEMANTIK (fail-closed, S3a): Default 25/50 ist ein KONKRETER Cap — jeder
-- Bestands-Tenant und jeder neue Tenant ist gedeckelt, NIE versehentlich unbegrenzt.
-- NULL = EXPLIZIT unbegrenzt (Operator-Akt). Der System/default-Tenant trägt die
-- Bestands-Keys (private/work/shared) und ist als einziger per Seed auf NULL =
-- unlimited gesetzt. Sane Defaults recherchiert: max_keys=50 (≈ Datadog Org-Limit),
-- max_scopes=25 (Knowledge-Tenant-Bedarf + Exhaustion-Cap). server-admin setzt sie
-- pro Tenant beim tenant-create/-limit-set (BEQ-1). 0 = bewusst "frozen".
--
-- ADD COLUMN ... DEFAULT (PG ≥ 11): konstanter Default = Metadata-only, KEIN Table-
-- Rewrite — Bestand bekommt 25/50 ohne Backfill.
--
-- Index-Tx-Trap (analog 063, db §1): der Runner hält jede Migration in EINER Tx →
-- CREATE INDEX CONCURRENTLY ist verboten. context_tenant_scopes + context_api_keys
-- sind heute klein (Onboarding-Scale), ein nicht-concurrent CREATE INDEX ist ein
-- kurzer Build. Bei echtem 1M-Volumen out-of-band vorab (manuell CREATE INDEX
-- CONCURRENTLY; die Migration findet ihn via IF NOT EXISTS). Tooling-Seam, keine
-- Schema-Frage. Die Indizes tragen die Cap-Counts (count WHERE tenant_id = $1) der
-- Slot-Reservierung, die sonst Seq-Scans über die ganze Child-Tabelle wären.
--
-- Idempotent (ADD COLUMN / CREATE INDEX IF NOT EXISTS, UPDATE auf NULL ist
-- konvergent, _migrations ON CONFLICT). Forward-only, self-registering.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- 1. Strukturelle Zähl-Limits auf dem Tenant. NULL-fähig (NULL = unlimited),
--    Default = konkreter fail-closed Cap, CHECK gegen Negativ-Werte.
ALTER TABLE context_tenants
    ADD COLUMN IF NOT EXISTS max_scopes INTEGER DEFAULT 25
        CHECK (max_scopes IS NULL OR max_scopes >= 0),
    ADD COLUMN IF NOT EXISTS max_keys   INTEGER DEFAULT 50
        CHECK (max_keys   IS NULL OR max_keys   >= 0);

-- 2. System/default-Tenant = unlimited (expliziter Akt; trägt die Bestands-Keys
--    private/work/shared, ist kein Self-Service-Mandant).
UPDATE context_tenants
    SET max_scopes = NULL, max_keys = NULL
    WHERE id = '00000000-0000-0000-0000-0000000d3fa0';

-- 3. Cap-Count-Zugriffspfade: count(*) WHERE tenant_id = $1 unter dem Slot-Lock.
CREATE INDEX IF NOT EXISTS idx_tenant_scopes_tenant
    ON context_tenant_scopes (tenant_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_tenant
    ON context_api_keys (tenant_id);

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (69, '069_tenant_limits.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 069_tenant_limits.sql

-- @@ ctx-fold begin 070_lifecycle_state_rename.sql
-- =============================================================================
-- 070_lifecycle_state_rename.sql — rename block_type → lifecycle_state
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Workflow-engine axis 01, wave T1 (design/01-type-registry.md §3.4, decision
-- D1). context_blocks carries two orthogonal type axes; this migration makes
-- the pipeline axis explicit by name:
--
--   block_type → lifecycle_state: WHERE the block stands in the pipeline
--   lifecycle (knowledge → canonical → snapshot; chunk; synthesis). The
--   column is written exclusively by mechanism code (dream promotion,
--   supersedes/revert, ingest chunks, daily report) — a closed, code-owned
--   state machine. The sibling rename block_role → type_name (the open,
--   registry-driven policy axis) follows in 071.
--
-- Steps:
--   1. RENAME COLUMN — metadata-only op, instant; indexes and constraints
--      (incl. the uq_source_chunk partial-index predicate from 012) track
--      attribute numbers and survive the rename unchanged.
--   2. NULL backfill → 'knowledge'. NULLs were produced by the historical
--      supersedes-revert (SET block_type = NULL); the Go side writes
--      'knowledge' from this wave on. The dead value 'source' (0 live rows)
--      is retired from all Go filters in the same wave — no data touch
--      needed for it.
--   3. SET NOT NULL — the column already has DEFAULT 'knowledge'; after the
--      backfill the constraint holds for all rows. Full-table validation
--      scan, trivially fast at current size and still a single short
--      ACCESS EXCLUSIVE hold at 1M+ rows (no rewrite).
--   4. ctx_guard_check DROP+CREATE — PL/pgSQL bodies resolve column names at
--      runtime, so without a re-create the first guard run after the rename
--      would fail with 42703. Body is the 011 version with ONLY the column
--      name adapted; thresholds/semantics unchanged (policy parametrisation
--      is wave T7's job, not this one's).
--
-- Historical migrations (011, 012, 042, …) keep referencing block_type: on a
-- fresh DB they run at their own position, BEFORE this rename — forward-only
-- ordering guarantees validity.
--
-- Idempotent (rename guarded by column-existence check, backfill convergent,
-- SET NOT NULL re-applicable, DROP+CREATE, _migrations ON CONFLICT).
-- Forward-only, self-registering.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- 1. Rename, guarded for idempotency: only if the old column still exists.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'context_blocks' AND column_name = 'block_type'
    ) THEN
        ALTER TABLE context_blocks RENAME COLUMN block_type TO lifecycle_state;
    END IF;
END $$;

-- 2. NULL backfill (historical supersedes-revert artefacts; 8 rows live at
--    migration authoring time).
UPDATE context_blocks SET lifecycle_state = 'knowledge' WHERE lifecycle_state IS NULL;

-- 3. Lock the invariant in: the lifecycle state machine has no NULL state.
ALTER TABLE context_blocks ALTER COLUMN lifecycle_state SET NOT NULL;

-- 4. Re-create ctx_guard_check with the new column name. 011 body,
--    name-adapted only — semantics identical (chunk exclusion, 0.98/0.92
--    thresholds, cross-scope logic untouched).
DROP FUNCTION IF EXISTS ctx_guard_check(UUID);

CREATE FUNCTION ctx_guard_check(p_block_id UUID)
RETURNS TABLE (
    decision        VARCHAR,
    top_similarity  NUMERIC,
    matched_id      UUID,
    matched_title   VARCHAR,
    matched_scope   VARCHAR,
    is_cross_scope  BOOLEAN
) LANGUAGE plpgsql AS $$
DECLARE
    v_embedding     vector(1024);
    v_scope         VARCHAR(20);
    v_matched_id    UUID;
    v_matched_title VARCHAR(255);
    v_matched_scope VARCHAR(20);
    v_similarity    NUMERIC;
BEGIN
    -- Load the block's embedding and scope
    SELECT cb.embedding, cb.scope
    INTO v_embedding, v_scope
    FROM context_blocks cb
    WHERE cb.id = p_block_id;

    -- If block not found or has no embedding, return clean with no match
    IF v_embedding IS NULL THEN
        decision       := 'clean';
        top_similarity := 0;
        matched_id     := NULL;
        matched_title  := NULL;
        matched_scope  := NULL;
        is_cross_scope := false;
        RETURN NEXT;
        RETURN;
    END IF;

    -- Find Top-1 nearest neighbor (excluding self, excluding archived, excluding chunks)
    SELECT
        cb.id,
        cb.title,
        cb.scope,
        round(
            (1 - (cb.embedding::halfvec(1024) <=> v_embedding::halfvec(1024)))::numeric,
            4
        )
    INTO v_matched_id, v_matched_title, v_matched_scope, v_similarity
    FROM context_blocks cb
    WHERE cb.id != p_block_id
      AND NOT cb.is_archived
      AND cb.embedding IS NOT NULL
      AND (cb.lifecycle_state IS NULL OR cb.lifecycle_state NOT IN ('chunk'))
    ORDER BY cb.embedding::halfvec(1024) <=> v_embedding::halfvec(1024)
    LIMIT 1;

    -- No neighbors found
    IF v_matched_id IS NULL THEN
        decision       := 'clean';
        top_similarity := 0;
        matched_id     := NULL;
        matched_title  := NULL;
        matched_scope  := NULL;
        is_cross_scope := false;
        RETURN NEXT;
        RETURN;
    END IF;

    -- Apply thresholds
    IF v_similarity >= 0.98 THEN
        decision := 'near_duplicate';
    ELSIF v_similarity >= 0.92 THEN
        decision := 'needs_review';
    ELSE
        decision := 'clean';
    END IF;

    -- Determine cross-scope status
    -- Cross-scope = match is NOT in same scope AND match is NOT shared
    top_similarity := v_similarity;
    matched_id     := v_matched_id;
    matched_title  := v_matched_title;
    matched_scope  := v_matched_scope;
    is_cross_scope := (v_matched_scope != v_scope AND v_matched_scope != 'shared');

    RETURN NEXT;
    RETURN;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (70, '070_lifecycle_state_rename.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 070_lifecycle_state_rename.sql

-- @@ ctx-fold begin 071_type_name_rename.sql
-- =============================================================================
-- 071_type_name_rename.sql — rename block_role → type_name + type_source
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Workflow-engine axis 01, wave T2 (design/01-type-registry.md §3.4, decision
-- D1). Sibling of 070 (block_type → lifecycle_state): this migration renames
-- the OPEN policy axis to its registry-era name:
--
--   block_role → type_name: WHAT the block is (knowledge, audit-trail,
--   reference, system-meta; future registry types: issue, comment, …). The
--   value set stays governed by the M035 CHECK constraint (its historical
--   name context_blocks_block_role_check survives the rename and only falls
--   in 073, together with the Go-side registry validation — never a deploy
--   state where neither DB nor Go validates the type names).
--
-- Steps:
--   1. RENAME COLUMN — metadata-only op, instant; the M035 partial index
--      (WHERE block_role != 'knowledge') and CHECK constraint track
--      attribute numbers and survive the rename (predicate text is
--      rewritten by PostgreSQL, constraint/index NAMES keep 'block_role').
--   2. type_source TEXT NOT NULL DEFAULT 'auto' — provenance column,
--      'auto' | 'manual', exact sensitivity_source pattern (055): a
--      'manual' value permanently overrides the auto-classifier (consumer
--      wiring is wave T4/T10; this wave only lays the column).
--   3. ctx_rrf DROP+CREATE — PL/pgSQL bodies resolve column names at
--      runtime, so without a re-create the first retrieval query after the
--      rename would fail with 42703. Body is the 068 version with ONLY the
--      column references adapted (cb.block_role → cb.type_name); signature,
--      parameter names (incl. p_block_roles_exclude — the wire field
--      block_roles_exclude stays compatible until T5/T10), CTE names and
--      semantics are byte-identical. Policy parametrisation is wave T5's
--      job (M073), not this one's.
--
-- Historical migrations (032, 035, 036, …) keep referencing block_role: on a
-- fresh DB they run at their own position, BEFORE this rename — forward-only
-- ordering guarantees validity.
--
-- Idempotent (rename guarded by column-existence check, ADD COLUMN IF NOT
-- EXISTS, DROP+CREATE, _migrations ON CONFLICT). Forward-only,
-- self-registering.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- 1. Rename, guarded for idempotency: only if the old column still exists.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'context_blocks' AND column_name = 'block_role'
    ) THEN
        ALTER TABLE context_blocks RENAME COLUMN block_role TO type_name;
    END IF;
END $$;

-- 2. Provenance column, sensitivity_source pattern (055): 'manual' wins
--    against the auto-classifier from T4 on. Backfill-free — every existing
--    row was classified by the Welle-44 hook or a migration, i.e. 'auto'.
ALTER TABLE context_blocks
    ADD COLUMN IF NOT EXISTS type_source TEXT NOT NULL DEFAULT 'auto'
        CHECK (type_source IN ('auto','manual'));

-- 3. Re-create ctx_rrf with the new column name. 068 body, column references
--    adapted only — signature and semantics identical (system-meta
--    hard-exclude, audit-trail damping via p_audit_trail_factor, grant
--    OR-arm parenthesisation untouched). Single DROP suffices: the new
--    signature equals the old one, so re-runs stay idempotent and exactly
--    ONE overload exists afterwards.
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT, DOUBLE PRECISION, TEXT[], TEXT[], UUID[]);

CREATE OR REPLACE FUNCTION ctx_rrf(
    p_embedding             halfvec(1024),
    p_query                 TEXT,
    p_query_spaced          TEXT,
    p_scopes                TEXT[],
    p_category              TEXT DEFAULT NULL,
    p_tags                  TEXT[] DEFAULT NULL,
    p_limit                 INT DEFAULT 5,
    p_temporal              TEXT DEFAULT NULL,
    p_query_or              TEXT DEFAULT NULL,
    p_audit_trail_factor    DOUBLE PRECISION DEFAULT 1.0,
    p_categories_exclude    TEXT[] DEFAULT NULL,
    p_block_roles_exclude   TEXT[] DEFAULT NULL,
    p_granted_block_ids     UUID[] DEFAULT NULL
) RETURNS TABLE (
    rrf_score   DOUBLE PRECISION,
    cosine_sim  DOUBLE PRECISION,
    id          UUID,
    title       TEXT,
    category    VARCHAR(100),
    tags        TEXT[],
    content     TEXT,
    scope       VARCHAR(50),
    updated_at  TIMESTAMPTZ
) LANGUAGE plpgsql AS $$
BEGIN
    SET LOCAL hnsw.iterative_scan = 'relaxed_order';

    RETURN QUERY
    WITH semantic AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
            ) AS rank,
            1.0 - (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS cos_sim
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name != 'system-meta'
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.type_name IS NULL OR cb.type_name != ALL(p_block_roles_exclude))
        ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
        LIMIT 75
    ),
    fulltext_de AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query)),
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name != 'system-meta'
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.type_name IS NULL OR cb.type_name != ALL(p_block_roles_exclude))
          AND (
              cb.ts_de @@ plainto_tsquery('german', p_query)
              OR cb.ts_de @@ plainto_tsquery('german', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    fulltext_en AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name != 'system-meta'
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.type_name IS NULL OR cb.type_name != ALL(p_block_roles_exclude))
          AND (
              cb.ts_en @@ plainto_tsquery('english', p_query)
              OR cb.ts_en @@ plainto_tsquery('english', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    trigram_title AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY similarity(cb.title, p_query) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name != 'system-meta'
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.type_name IS NULL OR cb.type_name != ALL(p_block_roles_exclude))
          AND similarity(cb.title, p_query) > 0.05
        LIMIT 30
    ),
    block_mass AS (
        SELECT cb.id,
               CASE
                   WHEN array_length(cb.content_times, 1) IS NULL THEN 1.0
                   WHEN array_length(cb.content_times, 1) = 0    THEN 1.0
                   ELSE 1.0 / sqrt(array_length(cb.content_times, 1)::DOUBLE PRECISION)
               END AS mass_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name != 'system-meta'
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.type_name IS NULL OR cb.type_name != ALL(p_block_roles_exclude))
    ),
    block_role_factor AS (
        SELECT cb.id,
               CASE
                   WHEN cb.type_name = 'audit-trail' THEN p_audit_trail_factor
                   ELSE 1.0
               END AS role_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name != 'system-meta'
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.type_name IS NULL OR cb.type_name != ALL(p_block_roles_exclude))
    ),
    rrf AS (
        SELECT
            COALESCE(s.id, d.id, e.id, g.id) AS block_id,
            (   0.45 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + s.rank), 0)
              + 0.20 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + d.rank), 0)
              + 0.25 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + e.rank), 0)
              + 0.10 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + g.rank), 0)
            )::DOUBLE PRECISION AS score,
            s.cos_sim::DOUBLE PRECISION
        FROM semantic s
        FULL OUTER JOIN fulltext_de d USING (id)
        FULL OUTER JOIN fulltext_en e USING (id)
        FULL OUTER JOIN trigram_title g USING (id)
        LEFT JOIN block_mass m ON m.id = COALESCE(s.id, d.id, e.id, g.id)
        LEFT JOIN block_role_factor rf ON rf.id = COALESCE(s.id, d.id, e.id, g.id)
    )
    SELECT
        r.score                    AS rrf_score,
        r.cos_sim                  AS cosine_sim,
        cb.id,
        cb.title::TEXT             AS title,
        cb.category,
        cb.tags,
        cb.content,
        cb.scope::VARCHAR(50)      AS scope,
        cb.updated_at
    FROM rrf r
    JOIN context_blocks cb ON cb.id = r.block_id
    ORDER BY r.score DESC
    LIMIT p_limit;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (71, '071_type_name_rename.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 071_type_name_rename.sql

-- @@ ctx-fold begin 072_block_type_registry.sql
-- =============================================================================
-- 072_block_type_registry.sql — dynamic block-type registry (workflow-engine
-- foundation). Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Workflow-engine axis 01, wave T3 (design/01-type-registry.md §3.2).
--
-- context_block_types: declarative per-type behaviour config. Mechanism stays
-- code (RRF fusion, guard similarity, dream loop, digest); HOW a type is
-- treated becomes data. Successor of the M035 block_role CHECK enum — the four
-- enum classes ship as builtin seed rows whose configs reproduce today's
-- hardcoded behaviour byte-equivalently (eval.sh is the gate). NOTE: no
-- consumer reads this table before wave T4+ — the M035 CHECK constraint on
-- context_blocks.type_name stays in force until 073 (fail-closed sequence,
-- design §3.4).
--
-- scope: '_global' sentinel = shipped/global namespace (F2/051 pattern; the
-- '_' prefix is enforced Go-side at api-key-create since M052). UNIQUE(name,
-- scope) carries all three tenancy tiers without a later constraint swap:
-- tier 1 global-only, tier 2 tenant overrides (tenant row shadows global row
-- of the same name), tier 3 tenant-own type names. Which tier is ENABLED is a
-- Go/actionTier concern, not a schema concern.
--
-- config JSONB, not typed columns: the policy vocabulary will grow with the
-- workflow engine (hooks envelope, board policy). Validation is Go-side
-- against a versioned schema (v2.0.0 line, M045: no hard value lists in the
-- schema). SQL never reads this JSONB — ctx_rrf/ctx_guard_check receive the
-- RESOLVED policy as bind parameters (policy-as-parameter pattern that
-- p_audit_trail_factor established in 039).
--
-- builtin rows: name+scope immutable, undeletable (Go-enforced); their config
-- IS editable (that is the point of the migration: damping factors, patterns
-- and thresholds become runtime-tunable). is_default: exactly one default
-- type per scope namespace (partial unique index) — the classifier falls back
-- to it when no rule matches.
--
-- updated_by deliberately without FK (051 audit line: references never
-- cascade; a key delete must not null history).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_block_types (
    id           UUID PRIMARY KEY DEFAULT uuidv7(),
    name         VARCHAR(50) NOT NULL,      -- Format-Gate in Go: ^[a-z0-9][a-z0-9-]{0,49}$
    scope        VARCHAR(50) NOT NULL DEFAULT '_global',
    display_name TEXT NOT NULL DEFAULT '',
    description  TEXT NOT NULL DEFAULT '',
    builtin      BOOLEAN NOT NULL DEFAULT false,
    is_default   BOOLEAN NOT NULL DEFAULT false,
    config       JSONB NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by   UUID,                      -- ohne FK (051-Linie)
    metadata     JSONB NOT NULL DEFAULT '{}',
    CONSTRAINT uq_block_types_name_scope UNIQUE (name, scope)
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_block_types_default
    ON context_block_types(scope) WHERE is_default;

-- Hot-Reload: derselbe Kanal wie Settings/Secrets/Backends/Quota.
-- notify_settings_write() (051, scope-erweitert 065) liest generisch
-- COALESCE(row->>'key', row->>'name') + row->>'scope' — passt für diese
-- Tabelle unverändert. Der Listener bekommt in T3 einen Entity-Branch
-- 'context_block_types' → Registry-Reload/InvalidateTenant (§4.3).
DROP TRIGGER IF EXISTS trg_block_types_notify ON context_block_types;
CREATE TRIGGER trg_block_types_notify
    AFTER INSERT OR UPDATE OR DELETE ON context_block_types
    FOR EACH ROW EXECUTE FUNCTION notify_settings_write();

-- Audit: eigene Trigger-Funktion (audit_settings_write() CASEt TG_TABLE_NAME
-- hart auf setting|secret — nicht wiederverwendbar ohne Umbau). Schreibt in
-- die bestehende generische History context_settings_audit (entity_type ist
-- CHECK-frei by design, 051-Kommentar). Type-Configs sind nie sensitiv →
-- old/new dürfen voll hinein.
--
-- Actor-Attribution (R1): current_setting('ctx.api_key_id') ist NUR gesetzt,
-- wenn der Go-Write in einer Tx mit setTxActor + SetTxRequestID läuft
-- (internal/store/settings.go; zweiter Nutzer: store/backends.go). T10 MUSS
-- alle type-*-Mutationen über dieses Muster verdrahten — plain pool.Exec
-- ergäbe via='sql', api_key_id NULL auf genau den Writes, die
-- Sichtbarkeits-Policy schalten (Provenienz-Verlust; das T10-Gate probt
-- via='api' positiv, design §7-T10).
CREATE OR REPLACE FUNCTION audit_block_type_write() RETURNS TRIGGER AS $$
DECLARE
    v_new   JSONB := to_jsonb(NEW);
    v_old   JSONB := to_jsonb(OLD);
    v_actor UUID  := NULLIF(current_setting('ctx.api_key_id', true), '')::uuid;
    v_label TEXT;
BEGIN
    IF v_actor IS NOT NULL THEN
        SELECT label INTO v_label FROM context_api_keys WHERE id = v_actor;
    END IF;
    INSERT INTO context_settings_audit
        (entity_type, entity_key, scope, action, old_value, new_value,
         api_key_id, actor_label, metadata)
    VALUES ('block_type',
        COALESCE(v_new->>'name', v_old->>'name'),
        COALESCE(v_new->>'scope', v_old->>'scope'),
        LOWER(TG_OP),
        v_old->'config', v_new->'config',
        v_actor, v_label,
        jsonb_strip_nulls(jsonb_build_object(
            'via', CASE WHEN v_actor IS NULL THEN 'sql' ELSE 'api' END,
            'request_id', NULLIF(current_setting('ctx.request_id', true), ''))));
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_block_types_audit ON context_block_types;
CREATE TRIGGER trg_block_types_audit
    AFTER INSERT OR UPDATE OR DELETE ON context_block_types
    FOR EACH ROW EXECUTE FUNCTION audit_block_type_write();

-- Seed: die 4 Enum-Klassen als builtin rows. Configs = heutiges Verhalten
-- BYTE-ÄQUIVALENT (Werte-Herkunft: Damping 0.3 = rrf.AuditTrailFactor;
-- Pattern-Liste = internal/rrf/pattern.go:25-36, 2× eingesetzt —
-- intent_patterns read-seitig, classify.title_patterns write-seitig;
-- guard.check/candidate=true überall = heutiger role-freier Guard-Batch;
-- dream.linkable=false nur system-meta = NOT is_meta; digest/overview
-- include=true überall = heutige Nicht-Filter; classify.priority 10 < 20 =
-- Reihenfolge des Decision-Trees internal/store/classify.go).
-- Das compiled-in Builtin-Set in internal/blocktype/builtin.go MUSS diesen
-- Seeds entsprechen — der Golden-Test appliziert diese Datei aus migrations.FS
-- und diff't die Rows gegen das Builtin-Set (Drift-Gate, design §4.1).
-- ON CONFLICT DO NOTHING = idempotent, überschreibt nie User-Tuning bei Re-Run.
INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config) VALUES
('knowledge', '_global', 'Knowledge', true, true, '{
  "v": 1,
  "retrieval": {"policy": "full-pass"},
  "guard":     {"check": true, "candidate": true},
  "dream":     {"linkable": true},
  "digest":    {"include": true},
  "overview":  {"include": true},
  "parent":    {"mode": "none"},
  "classify":  {}
}'::jsonb),
('reference', '_global', 'Reference', true, false, '{
  "v": 1,
  "retrieval": {"policy": "full-pass"},
  "guard":     {"check": true, "candidate": true},
  "dream":     {"linkable": true},
  "digest":    {"include": true},
  "overview":  {"include": true},
  "parent":    {"mode": "none"},
  "classify":  {}
}'::jsonb),
('audit-trail', '_global', 'Audit-Trail', true, false, '{
  "v": 1,
  "retrieval": {"policy": "damped", "damping_factor": 0.3,
                "intent_patterns": ["session","welle","audit","recurrent","handover",
                                    "self-audit","dream v","performance","reset","baseline"]},
  "guard":     {"check": true, "candidate": true},
  "dream":     {"linkable": true},
  "digest":    {"include": true},
  "overview":  {"include": true},
  "parent":    {"mode": "none"},
  "classify":  {"priority": 20,
                "source_prefixes": ["dream-"],
                "title_patterns": ["session","welle","audit","recurrent","handover",
                                   "self-audit","dream v","performance","reset","baseline"]}
}'::jsonb),
('system-meta', '_global', 'System-Meta', true, false, '{
  "v": 1,
  "retrieval": {"policy": "excluded"},
  "guard":     {"check": true, "candidate": true},
  "dream":     {"linkable": false},
  "digest":    {"include": true},
  "overview":  {"include": true},
  "parent":    {"mode": "none"},
  "classify":  {"priority": 10, "metadata_flags": ["is_meta"]}
}'::jsonb)
ON CONFLICT (name, scope) DO NOTHING;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (72, '072_block_type_registry.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 072_block_type_registry.sql

-- @@ ctx-fold begin 073_rrf_policy_params.sql
-- =============================================================================
-- 073_rrf_policy_params.sql — ctx_rrf policy-parametrisiert + CHECK-Drop
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Workflow-engine axis 01, wave T5 (design/01-type-registry.md §3.5). The
-- 14th ctx_rrf generation: type visibility and damping become PARAMETERS fed
-- from the block-type registry (M072) — the last hardcoded type literals in
-- the retrieval path fall.
--
-- THE semantic switch of the axis (fail-open → fail-closed, §5.1a):
--
--   068/071:  cb.type_name != 'system-meta'          -- EXCLUDE literal.
--             An unregistered ("rogue") type name is NOT in the exclude
--             list ⇒ full-pass. Fail-OPEN.
--   073:      cb.type_name = ANY(p_types_visible)    -- ALLOWLIST parameter.
--             Only registered, retrieval-visible types pass. A rogue
--             type name (SQL INSERT past Go) is invisible until a
--             registry row carries it. NULL/empty allowlist ⇒ 0 rows —
--             deliberately hard; Go guarantees never-NULL analogous to
--             the len(scopes)==0 reject (rrf/search.go).
--
-- Damping generalised (replaces block_role_factor + p_audit_trail_factor):
-- parallel arrays p_damped_types/p_damped_factors (unnest join). The intent
-- lift moved to Go (blocktype.Set.DampedTypesFor): a lifted type is simply
-- absent from the arrays ⇒ COALESCE factor 1.0. p_audit_trail_factor is
-- gone WITHOUT replacement (its semantics live in the arrays).
--
-- Parenthesisation invariant (068 header, §3.5 invariant 2): the archived +
-- BOTH type conjuncts stand strictly BEFORE the (scope OR grant) parens — a
-- granted block with an excluded/rogue type stays invisible (§5.3 probe).
--
-- RETURNS gains type_name (UI badges / aggregate-to-parent fold need the
-- type on every result row — seam §2.8; wire exposure is T10's job).
--
-- CHECK-Drop: context_blocks_block_role_check (M035 enum, name survived the
-- 071 column rename) falls — the registry is the authority on type names
-- now (Go write validation T10 + allowlist read path here). A rogue INSERT
-- past Go becomes POSSIBLE and is WANTED that way: fail-closed lives in the
-- read path, observability in the orphan sweep (§5.1a), not in a hard enum
-- that would block every new workflow type.
--
-- Wire compat: the REST field block_roles_exclude stays (seam 17; rename
-- alias types_exclude is T10) — Go maps it onto p_types_exclude.
--
-- Function-only + constraint drop, no table/column change: test.sh T07
-- table/column counts unchanged. Idempotent (DROP IF EXISTS both
-- signatures + CREATE OR REPLACE + ON CONFLICT DO NOTHING), forward-only,
-- self-registering (M031+ convention). On a fresh DB 048/068/071 run at
-- their positions first; this file replaces the function afterwards.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- 1. The M035 enum CHECK falls: the registry is the type-name authority.
ALTER TABLE context_blocks DROP CONSTRAINT IF EXISTS context_blocks_block_role_check;

-- 2. Drop the 071 signature (and the new one for idempotent re-runs) —
--    M048/068 pattern: CREATE OR REPLACE alone would leave the old overload
--    alive alongside the new one (42725 ambiguity for legacy callers).
--    ZOMBIE SWEEP (T5 finding): the 003 (7-param) and 006 (8-param)
--    generations were never dropped by ANY later migration — 020 removed
--    the DATE variants, 030-039 the 9-param one, but these two survived the
--    whole chain (fresh DB: `\df ctx_rrf` = 3 signatures pre-073). They are
--    dead weight and an overload-resolution hazard for short positional
--    calls; the T5 single-signature gate flushes them here.
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT);
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT);
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT, DOUBLE PRECISION, TEXT[], TEXT[], UUID[]);
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT, TEXT[], TEXT[], DOUBLE PRECISION[], TEXT[], TEXT[], UUID[]);

CREATE OR REPLACE FUNCTION ctx_rrf(
    p_embedding             halfvec(1024),
    p_query                 TEXT,
    p_query_spaced          TEXT,
    p_scopes                TEXT[],
    p_category              TEXT DEFAULT NULL,
    p_tags                  TEXT[] DEFAULT NULL,
    p_limit                 INT DEFAULT 5,
    p_temporal              TEXT DEFAULT NULL,
    p_query_or              TEXT DEFAULT NULL,
    p_types_visible         TEXT[] DEFAULT NULL,   -- ALLOWLIST (fail-closed): NULL/leer ⇒ 0 Treffer
    p_damped_types          TEXT[] DEFAULT NULL,   -- parallel zu p_damped_factors
    p_damped_factors        DOUBLE PRECISION[] DEFAULT NULL,
    p_categories_exclude    TEXT[] DEFAULT NULL,
    p_types_exclude         TEXT[] DEFAULT NULL,   -- Request-Level-Exclude (vorm. p_block_roles_exclude)
    p_granted_block_ids     UUID[] DEFAULT NULL
) RETURNS TABLE (
    rrf_score   DOUBLE PRECISION,
    cosine_sim  DOUBLE PRECISION,
    id          UUID,
    title       TEXT,
    category    VARCHAR(100),
    tags        TEXT[],
    content     TEXT,
    scope       VARCHAR(50),
    updated_at  TIMESTAMPTZ,
    type_name   TEXT
) LANGUAGE plpgsql AS $$
BEGIN
    SET LOCAL hnsw.iterative_scan = 'relaxed_order';

    RETURN QUERY
    WITH semantic AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
            ) AS rank,
            1.0 - (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS cos_sim
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
        ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
        LIMIT 75
    ),
    fulltext_de AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query)),
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (
              cb.ts_de @@ plainto_tsquery('german', p_query)
              OR cb.ts_de @@ plainto_tsquery('german', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    fulltext_en AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (
              cb.ts_en @@ plainto_tsquery('english', p_query)
              OR cb.ts_en @@ plainto_tsquery('english', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    trigram_title AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY similarity(cb.title, p_query) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND similarity(cb.title, p_query) > 0.05
        LIMIT 30
    ),
    block_mass AS (
        SELECT cb.id,
               CASE
                   WHEN array_length(cb.content_times, 1) IS NULL THEN 1.0
                   WHEN array_length(cb.content_times, 1) = 0    THEN 1.0
                   ELSE 1.0 / sqrt(array_length(cb.content_times, 1)::DOUBLE PRECISION)
               END AS mass_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
    ),
    type_factor AS (
        SELECT cb.id, COALESCE(f.factor, 1.0) AS factor
        FROM context_blocks cb
        LEFT JOIN unnest(p_damped_types, p_damped_factors) AS f(tname, factor)
               ON cb.type_name = f.tname
        WHERE NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
    ),
    rrf AS (
        SELECT
            COALESCE(s.id, d.id, e.id, g.id) AS block_id,
            (   0.45 * COALESCE(m.mass_factor, 1.0) * COALESCE(tf.factor, 1.0) * COALESCE(1.0 / (60 + s.rank), 0)
              + 0.20 * COALESCE(m.mass_factor, 1.0) * COALESCE(tf.factor, 1.0) * COALESCE(1.0 / (60 + d.rank), 0)
              + 0.25 * COALESCE(m.mass_factor, 1.0) * COALESCE(tf.factor, 1.0) * COALESCE(1.0 / (60 + e.rank), 0)
              + 0.10 * COALESCE(m.mass_factor, 1.0) * COALESCE(tf.factor, 1.0) * COALESCE(1.0 / (60 + g.rank), 0)
            )::DOUBLE PRECISION AS score,
            s.cos_sim::DOUBLE PRECISION
        FROM semantic s
        FULL OUTER JOIN fulltext_de d USING (id)
        FULL OUTER JOIN fulltext_en e USING (id)
        FULL OUTER JOIN trigram_title g USING (id)
        LEFT JOIN block_mass m ON m.id = COALESCE(s.id, d.id, e.id, g.id)
        LEFT JOIN type_factor tf ON tf.id = COALESCE(s.id, d.id, e.id, g.id)
    )
    SELECT
        r.score                    AS rrf_score,
        r.cos_sim                  AS cosine_sim,
        cb.id,
        cb.title::TEXT             AS title,
        cb.category,
        cb.tags,
        cb.content,
        cb.scope::VARCHAR(50)      AS scope,
        cb.updated_at,
        cb.type_name::TEXT         AS type_name
    FROM rrf r
    JOIN context_blocks cb ON cb.id = r.block_id
    ORDER BY r.score DESC
    LIMIT p_limit;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (73, '073_rrf_policy_params.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 073_rrf_policy_params.sql

-- @@ ctx-fold begin 074_guard_check_type_policy.sql
-- =============================================================================
-- 074_guard_check_type_policy.sql — ctx_guard_check parametrisiert + Guard-Pending-Index
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Workflow-engine axis 01, wave T7 (design/01-type-registry.md §3.6) MERGED
-- with the axis-02 K1 share (design/02-issue-workflow.md §4.7/§9.1): ONE
-- unified ctx_guard_check generation in ONE DROP+CREATE — never two.
--
-- Axis 01 share (§3.6): thresholds + candidate set become PARAMETERS fed from
-- the block-type registry (M072). All three are MANDATORY — NO defaults:
--
--   * p_threshold_duplicate / p_threshold_review — the 0.98/0.92 literals of
--     the 011 line leave the function body; Go resolves them per BLOCK TYPE
--     (blocktype.Set.GuardThresholds).
--   * p_candidate_types — the candidate allowlist (guard.candidate=true
--     types). NULL ⇒ 0 candidates (`= ANY(NULL)` is never true — hard,
--     analogous to p_types_visible in 073). A DEFAULT NULL with "legacy =
--     all types" would be a silent policy bypass (§5.4 actionTier lesson);
--     a legacy 1-arg call therefore fails LOUDLY with 42883.
--
-- Axis 02 share (02 §4.7 — the K1 Issue-Guard order):
--
--   * p_same_scope_only — TRUE restricts the candidate set to the checked
--     block's own scope (issue dedup must never match cross-tenant/scope,
--     02 §5.3). DEFAULT FALSE: 02 §4.7 orders the default explicitly (its
--     "1-arg rollback" rationale predates the 01-R1 no-defaults decision and
--     is void — the 1-arg call is 42883 now — but the FALSE default remains
--     correct: cross-scope stays the knowledge-line semantic, and Go passes
--     the parameter EXPLICITLY at every call site regardless; the I-J gate
--     enumerates call sites against <5-arg calls).
--   * Same-scope branch sets hnsw.iterative_scan='relaxed_order' (filtered
--     ANN, 02 §4.7): at 1M+ blocks a ≤1%-selectivity scope predicate can
--     exhaust the ef_search candidates without a single same-scope hit —
--     the guard would silently report clean. relaxed_order makes pgvector
--     iterate the graph until the predicate yields candidates (house
--     pattern: ctx_rrf since 003/006, last 068/073). The cross-scope branch
--     keeps the 011-line unfiltered top-1 (no GUC) — byte-identical
--     behaviour under seed defaults.
--
-- Body deltas vs 070 (the 011 line): candidate filter gains
-- `cb.lifecycle_state != 'chunk'` (NOT-NULL since 070 — the NULL arm of the
-- old predicate is dead) AND `cb.type_name = ANY(p_candidate_types)` AND the
-- optional scope conjunct; the threshold CASEs consume the parameters.
-- v_scope/v_matched_scope widened VARCHAR(20)→VARCHAR(50): scope has been
-- VARCHAR(50) since 047 — the old declarations were a latent 22001
-- truncation error for long scopes, never a behaviour change for short ones.
--
-- idx_guard_pending (§3.6 R1 — the missing guard-pending index): the pick
-- predicate `(metadata->>'guard_checked_at') IS NULL` (guard.go) and the two
-- count subqueries had NO carrying index — GIN(metadata) (jsonb_ops) does not
-- serve `->> IS NULL`, and idx_context_created only carries the ORDER BY
-- (heap-filtering every row). At 1M+ blocks that is 2-3 full scans per guard
-- batch, and forge-sync bursts (10k+ issues/repo) trigger batches via
-- NotifyWrite exactly then. Partial index after the idx_dream_pending pattern
-- (016): only pending rows are IN the index, so it stays small while the
-- table grows. Predicate = the three conjuncts all three guard queries share;
-- category != 'index', the type allowlist and (T12) the scope conjunct filter
-- on the index result.
--
-- Function + index only, no table/column change: test.sh T07 table/column
-- counts unchanged. Idempotent (DROP IF EXISTS both signatures + CREATE +
-- IF NOT EXISTS + ON CONFLICT DO NOTHING), forward-only, self-registering
-- (M031+ convention).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE INDEX IF NOT EXISTS idx_guard_pending
    ON context_blocks (created_at ASC)
    WHERE NOT is_archived
      AND (metadata->>'guard_checked_at') IS NULL
      AND embedding IS NOT NULL;

-- DROP+CREATE (house pattern 068:48-50, 02 §9.1): CREATE OR REPLACE with a
-- changed parameter list would leave the 1-arg overload alive → 42725
-- ambiguity; the old signature must FALL so the legacy call is a loud 42883.
DROP FUNCTION IF EXISTS ctx_guard_check(UUID);
DROP FUNCTION IF EXISTS ctx_guard_check(UUID, REAL, REAL, TEXT[], BOOLEAN);

CREATE FUNCTION ctx_guard_check(
    p_block_id            UUID,
    p_threshold_duplicate REAL,                   -- Pflicht (01 §3.6, kein Default)
    p_threshold_review    REAL,                   -- Pflicht (01 §3.6, kein Default)
    p_candidate_types     TEXT[],                 -- Pflicht; NULL ⇒ 0 Kandidaten (fail-closed)
    p_same_scope_only     BOOLEAN DEFAULT FALSE   -- Achse-02-Anteil (02 §4.7, K1/I-J)
)
RETURNS TABLE (
    decision        VARCHAR,
    top_similarity  NUMERIC,
    matched_id      UUID,
    matched_title   VARCHAR,
    matched_scope   VARCHAR,
    is_cross_scope  BOOLEAN
) LANGUAGE plpgsql AS $$
DECLARE
    v_embedding     vector(1024);
    v_scope         VARCHAR(50);
    v_matched_id    UUID;
    v_matched_title VARCHAR(255);
    v_matched_scope VARCHAR(50);
    v_similarity    NUMERIC;
BEGIN
    -- Load the block's embedding and scope
    SELECT cb.embedding, cb.scope
    INTO v_embedding, v_scope
    FROM context_blocks cb
    WHERE cb.id = p_block_id;

    -- If block not found or has no embedding, return clean with no match
    IF v_embedding IS NULL THEN
        decision       := 'clean';
        top_similarity := 0;
        matched_id     := NULL;
        matched_title  := NULL;
        matched_scope  := NULL;
        is_cross_scope := false;
        RETURN NEXT;
        RETURN;
    END IF;

    -- Same-scope dedup is filtered ANN (02 §4.7): iterate the HNSW graph
    -- until the scope predicate yields candidates instead of returning a
    -- silent false-clean. LOCAL: resets at transaction end; the cross-scope
    -- branch deliberately sets nothing (011-line plan, byte-identical).
    IF p_same_scope_only THEN
        SET LOCAL hnsw.iterative_scan = 'relaxed_order';
    END IF;

    -- Find Top-1 nearest neighbor (excluding self, excluding archived,
    -- excluding chunks) — restricted to the POLICY candidate set
    -- (p_candidate_types allowlist; NULL ⇒ `= ANY(NULL)` never true ⇒ 0
    -- candidates, fail-closed) and, for the issue axis, to the own scope.
    SELECT
        cb.id,
        cb.title,
        cb.scope,
        round(
            (1 - (cb.embedding::halfvec(1024) <=> v_embedding::halfvec(1024)))::numeric,
            4
        )
    INTO v_matched_id, v_matched_title, v_matched_scope, v_similarity
    FROM context_blocks cb
    WHERE cb.id != p_block_id
      AND NOT cb.is_archived
      AND cb.embedding IS NOT NULL
      AND cb.lifecycle_state != 'chunk'
      AND cb.type_name = ANY(p_candidate_types)
      AND (NOT p_same_scope_only OR cb.scope = v_scope)
    ORDER BY cb.embedding::halfvec(1024) <=> v_embedding::halfvec(1024)
    LIMIT 1;

    -- No neighbors found
    IF v_matched_id IS NULL THEN
        decision       := 'clean';
        top_similarity := 0;
        matched_id     := NULL;
        matched_title  := NULL;
        matched_scope  := NULL;
        is_cross_scope := false;
        RETURN NEXT;
        RETURN;
    END IF;

    -- Apply thresholds (policy parameters — the 011 literals left the body)
    IF v_similarity >= p_threshold_duplicate THEN
        decision := 'near_duplicate';
    ELSIF v_similarity >= p_threshold_review THEN
        decision := 'needs_review';
    ELSE
        decision := 'clean';
    END IF;

    -- Determine cross-scope status
    -- Cross-scope = match is NOT in same scope AND match is NOT shared
    top_similarity := v_similarity;
    matched_id     := v_matched_id;
    matched_title  := v_matched_title;
    matched_scope  := v_matched_scope;
    is_cross_scope := (v_matched_scope != v_scope AND v_matched_scope != 'shared');

    RETURN NEXT;
    RETURN;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (74, '074_guard_check_type_policy.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 074_guard_check_type_policy.sql

-- @@ ctx-fold begin 075_drop_is_meta.sql
-- =============================================================================
-- 075_drop_is_meta.sql — drop the materialised is_meta column + its index
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Workflow-engine axis 01, wave T9 (design/01-type-registry.md §3.7/§7-T9).
-- The third type axis is consolidated: is_meta (M029, empirical dream-noise
-- exclude) was live congruent with type_name='system-meta' (verified on the
-- production corpus 2026-07-02: 24/24 rows, 0 asymmetric of 1141), and its
-- only behavioural effect — dream exclusion on both the pick and the
-- candidate/target side — moved into the registry policy `dream.linkable`
-- (false on the system-meta seed) in wave T8. Since T8 no code READS the
-- column (the five eligibility mirrors + the candidate batch-lookup consume
-- the DreamLinkableTypes allowlist); since this wave the classify hook no
-- longer WRITES it either (store/classify.go UPDATE carries type_name only).
--
--   metadata.is_meta (the JSONB KEY) deliberately survives: it is a classify
--   INPUT (the system-meta seed's classify.metadata_flags rule fires on it).
--   Only the materialised column falls.
--
-- Steps:
--   1. DROP INDEX idx_blocks_is_meta (M029's partial picker index — carried
--      only the retired NOT-is_meta dream-eligibility scans; dropped BEFORE
--      the column so the statement order reads as intent, though DROP COLUMN
--      would cascade it anyway).
--   2. DROP COLUMN is_meta.
--
-- Both steps are metadata-only catalog ops (no table rewrite): a short
-- ACCESS EXCLUSIVE lock, trivially safe at 1M+ rows; the reclaimed bytes are
-- reused by future row versions (standard PG column drop semantics).
--
-- Historical migrations (029, 036, 044, …) keep referencing is_meta: on a
-- fresh DB they run at their own position, BEFORE this drop — forward-only
-- ordering guarantees validity (the M070/M071 rename line's precedent).
--
-- Idempotent (IF EXISTS on both drops, _migrations ON CONFLICT).
-- Forward-only, self-registering.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

DROP INDEX IF EXISTS idx_blocks_is_meta;

ALTER TABLE context_blocks DROP COLUMN IF EXISTS is_meta;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (75, '075_drop_is_meta.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 075_drop_is_meta.sql

-- @@ ctx-fold begin 076_structural_links.sql
-- =============================================================================
-- 076_structural_links.sql — deterministic structural link layer (Achse 02, I-A)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Deterministic, forge-/system-derived relations between blocks. STRICTLY
-- SEPARATE from context_dream_links: dream edges are *discovered* (LLM,
-- confidence-gated; the replace-semantics of writelinks.go:38-78 delete
-- foreign-origin rows of a source), structural edges are *facts* (confidence
-- 1.0 by definition — hence NO confidence column). Vision anchor
-- 019e83df-9666: "Strukturelle Links NICHT in den Dream-Graph mischen".
--   K3 (00-masterplan §2): parent_id + FK CASCADE = the ONE structural parent
--   (comment-of); context_structural_links = all further structural classes
--   (references, duplicate-of, tracks, blocks-issue, …). dream_links stays
--   purely semantic; Dream replace/cleanup never touches this table
--   (negative gate I-A: dream.WriteLinks replace leaves structural edges intact).
--
-- link_class carries NO CHECK (M045 line: validation is a runtime concern — the
-- Achse-01 type registry validates classes in Go via structural_link_classes).
-- v1 seed classes: 'references' (issue→issue cross-ref, body parsing §4.5.7) and
-- 'duplicate-of' (manual / guard-flag confirmation). 'subissue-of', 'pr-linked',
-- 'milestone-of' are BACKLOG (not materialized as blocks / cost per-issue API
-- calls §6.1) — dead vocabulary is not seeded. comment→issue lives on
-- context_blocks.parent_id (001:39), NOT here — one fact, one place.
--
-- Same-scope is enforced app-side in the SAME Tx as the write (§4.2/§5.2):
-- source and target MUST share one scope; the scope column IS that scope.
-- ON DELETE CASCADE (unlike 016's bare FK): tenant prune batch-deletes blocks
-- (store/tenant.go:474-480) and must not gain a new drain step for THIS table;
-- a deliberate scope index avoids repeating the N11 seam (tenant.go:462-473).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_structural_links (
    source_block_id UUID NOT NULL REFERENCES context_blocks(id) ON DELETE CASCADE,
    target_block_id UUID NOT NULL REFERENCES context_blocks(id) ON DELETE CASCADE,
    link_class      TEXT NOT NULL,
    scope           VARCHAR(50) NOT NULL,          -- == Source-Scope == Target-Scope (Tx-validiert, §4.2)
    origin          TEXT NOT NULL DEFAULT 'forge-sync',  -- forge-sync | manual | system
    metadata        JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (source_block_id, target_block_id, link_class),
    CHECK (source_block_id != target_block_id)
);

-- Reverse traversal (issue-get / EgoGraph UNION leg): "who points AT me".
CREATE INDEX IF NOT EXISTS idx_struct_links_rev
    ON context_structural_links (target_block_id, link_class)
    INCLUDE (source_block_id);

-- Scope index from day 1 (N11 lesson, tenant.go:462-473) — @1M+ blocks a
-- scope-scan without it would seq-scan the whole edge table.
CREATE INDEX IF NOT EXISTS idx_struct_links_scope
    ON context_structural_links (scope);

-- parent_id-FK (E8): Achse-01-Annahme "+FK/Index durch Achse 02" (01 §9.1b).
-- The parent_id COLUMN already exists (001:39) and is indexed (idx_parent_id
-- 001:204) — this migration adds ONLY the FK constraint, no column, so
-- context_blocks column count is UNCHANGED (T07 oracle: cols stay 39).
-- NOT VALID + VALIDATE: no full-table-lock scan gated by the constraint add;
-- all parent_id are live NULL (grep: null Konsumenten), VALIDATE is a nullpass.
-- ON DELETE CASCADE: Issue-Delete räumt seine Comments; prune-kompatibel — the
-- batched ctid-deletes (tenant.go:474-480) treffen Parent und Kinder ohnehin
-- (gleicher Scope, Invariante §5.2), CASCADE macht die Reihenfolge egal.
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_blocks_parent') THEN
    ALTER TABLE context_blocks
      ADD CONSTRAINT fk_blocks_parent FOREIGN KEY (parent_id)
      REFERENCES context_blocks(id) ON DELETE CASCADE NOT VALID;
  END IF;
END $$;
ALTER TABLE context_blocks VALIDATE CONSTRAINT fk_blocks_parent;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (76, '076_structural_links.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 076_structural_links.sql

-- @@ ctx-fold begin 077_workflow_status.sql
-- =============================================================================
-- 077_workflow_status.sql — generic per-block workflow state (Achse 02, I-B)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- VALUE per block = column (mechanism); the SET of valid states, transitions,
-- board order and terminal flags = Achse-01 type config (policy=data). The Go
-- transition validator (blocktype.Set.ValidateTransition) enforces the per-type
-- state machine against the registry — a CHECK constraint would hard-couple the
-- schema to policy (M045 line: validation is a runtime concern, no CHECK).
--
-- Nullable, NO backfill: a non-workflow type stays NULL at zero storage cost
-- (PG null bitmap). NULL is the correct semantics "this block has no workflow"
-- for the entire knowledge corpus — lifting an existing type into workflow
-- semantics is a later, idempotent, own-wave UPDATE (§3.3).
--
-- Column name type_name per D1 (01 §3.4) — the policy axis; it already carries
-- the issue/comment discriminator (migration 071).
--
-- INDEX NOTE (063/069 house norm, tenant.go N11 comment): plain CREATE INDEX in
-- the single-Tx runner (store/migrations.go, CONCURRENTLY forbidden) holds a
-- SHARE lock for the build. Fine at today's ~1k rows; at 1M+ rows build the
-- index OUT-OF-BAND first with
--   CREATE INDEX CONCURRENTLY idx_blocks_workflow_board ...
-- and the IF NOT EXISTS below then finds it — this migration becomes a no-op.
-- Deploy-Runbook-Eintrag: §9.5.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE context_blocks
    ADD COLUMN IF NOT EXISTS workflow_status VARCHAR(50);

-- Board-/Listen-Pfad @10k+ Issues pro Repo: keyset-fähig, partial (nur
-- Workflow-Rows). Equality on (scope, type_name, workflow_status) plus the
-- keyset ordering (updated_at DESC, id DESC) makes ONE ordered index range scan
-- per board column — Sort-free with a row-comparison cursor (updated_at, id) <
-- (cur_updated, cur_id). The status-UNGEFILTERTE Liste runs as a per-status
-- merge in Go (one range scan per config status, k-way merge), never a Sort over
-- the whole scope (§3.3 Listen-Semantik, §6.2).
--
-- DEVIATION from the §3.3 sketch: the sketch wrote `... updated_at DESC, id`
-- (id ASC). A mixed-direction keyset (updated_at DESC, id ASC) is NOT a single
-- ordered btree range — Postgres plans it as a BitmapOr of the OR-form keyset
-- predicate + a top-N Sort (measured). id DESC makes the cursor a clean tuple
-- comparison matching the index direction ⇒ ordered range scan, no Sort. id is
-- only a unique tie-break, so the direction is semantically free (§7-I-B gate).
CREATE INDEX IF NOT EXISTS idx_blocks_workflow_board
    ON context_blocks (scope, type_name, workflow_status, updated_at DESC, id DESC)
    WHERE workflow_status IS NOT NULL AND NOT is_archived;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (77, '077_workflow_status.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 077_workflow_status.sql

-- @@ ctx-fold begin 078_write_scopes.sql
-- =============================================================================
-- 078_write_scopes.sql — explicit per-key write scopes (Workflow-Achse W3, E4=b)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Adds context_api_keys.write_scopes — an explicit, per-key set of scopes the
-- key may WRITE blocks to, over and above its home_scope (+ shared-when-allowed).
-- Design: design/03-workflow-api-cli.md §3.3 (provisional M074, final number 078
-- per masterplan §2 K1); decision E4=b (DECISIONS.md).
--
-- SHAPE: TEXT[] NOT NULL DEFAULT '{}', identical to allowed_scopes (058/052 line)
-- — same array type, same '_'-reserved namespace, same T22 tenant rules. NOT
-- JSONB: allowed_scopes is TEXT[], and the write gate intersects the two arrays
-- in Go (writableBlockScopes), so keeping one array type avoids a cast layer.
--
-- BACKFILL: none. DEFAULT '{}' makes writableBlockScopes byte-identical for every
-- existing key (home_scope ∪ {shared-when-allowed}); the shared special case is
-- untouched. This is the pausability/rollback invariant: an old binary ignores
-- the column entirely, a new binary with an empty column reproduces v4.2.x exactly.
--
-- INVARIANT write_scopes ⊆ allowed_scopes ∪ {home_scope} is enforced DOUBLE, both
-- in Go (no DB CHECK — v2.0.0 line, open sets are validated in code): (a) at mint/
-- update time (api-key-create / api-key-update reject a write_scope with no read
-- right — a blind-writer), and (b) at the SINGLE eval point writableBlockScopes,
-- whose formula intersects write_scopes with (allowed_scopes ∪ home_scope) so a
-- stale entry left by a later allowed_scopes shrink is neutralised for free — one
-- fail-closed eval point, not N write sites (design §3.3 / §5.1).
--
-- ctx_auth is RETURNS TABLE, so a new return column forces DROP+CREATE (052/060
-- line; OR REPLACE refuses a return-type change). write_scopes is appended AS THE
-- LAST column so every named-column SELECT (auth.go) and AuthResult{} literal stays
-- valid; an old binary that selects the original 8 columns keeps working. The body
-- is byte-for-byte the 060 body plus the one new column — no behavioural change to
-- identity, the tenant status gate, or the positional read_scopes build.
--
-- lock_timeout (R-MIG2): ADD COLUMN with a constant NOT NULL DEFAULT is a
-- catalog-only, non-rewriting change on PG11+ (the default is stored in
-- pg_attribute, no table rewrite); DROP/CREATE FUNCTION takes only brief catalog
-- locks. The runner wraps each migration in its own transaction, so SET LOCAL is
-- transaction-scoped and self-reverting.
-- Idempotent: ADD COLUMN IF NOT EXISTS + DROP FUNCTION IF EXISTS + CREATE re-run
-- cleanly; _migrations INSERT ON CONFLICT (version) DO NOTHING. Forward-only.
-- Function-only body change + one additive column → test.sh table count UNCHANGED
-- (no new table; context_api_keys column count is not pinned by test.sh).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE context_api_keys
    ADD COLUMN IF NOT EXISTS write_scopes TEXT[] NOT NULL DEFAULT '{}';

DROP FUNCTION IF EXISTS ctx_auth(TEXT);

CREATE FUNCTION ctx_auth(p_api_key TEXT)
RETURNS TABLE (
    api_key_id     UUID,
    home_scope     VARCHAR(50),
    allowed_scopes TEXT[],
    read_scopes    TEXT[],
    is_valid       BOOLEAN,
    is_admin       BOOLEAN,
    tenant_id      UUID,
    tenant_role    TEXT,
    write_scopes   TEXT[]    -- NEW (W3, E4b): explicit per-key write scopes; RAW
                             -- column value — the ⊆ intersection is applied in Go
) LANGUAGE plpgsql AS $$
DECLARE
    v_key_hash       TEXT;
    v_api_key_id     UUID;
    v_home_scope     VARCHAR(50);
    v_allowed_scopes TEXT[];
    v_is_admin       BOOLEAN;
    v_tenant_id      UUID;
    v_tenant_role    TEXT;
    v_status         TEXT;
    v_read_scopes    TEXT[];
    v_cand           TEXT[];
    v_s              TEXT;
    v_write_scopes   TEXT[];      -- NEW
BEGIN
    v_key_hash := encode(digest(p_api_key, 'sha256'), 'hex');

    UPDATE context_api_keys
    SET last_used_at = now()
    WHERE context_api_keys.key_hash = v_key_hash
      AND context_api_keys.active = true
    RETURNING
        context_api_keys.id,
        context_api_keys.home_scope,
        context_api_keys.allowed_scopes,
        context_api_keys.is_admin,
        context_api_keys.tenant_id,
        context_api_keys.tenant_role,
        context_api_keys.write_scopes
    INTO v_api_key_id, v_home_scope, v_allowed_scopes, v_is_admin, v_tenant_id, v_tenant_role, v_write_scopes;

    -- Key miss: sentinel (unchanged shape; +1 explicit new column).
    IF v_api_key_id IS NULL THEN
        api_key_id     := NULL;
        home_scope     := '__UNAUTHORIZED__';
        allowed_scopes := '{}'::TEXT[];
        read_scopes    := '{}'::TEXT[];
        is_valid       := false;
        is_admin       := false;
        tenant_id      := NULL;
        tenant_role    := '';
        write_scopes   := '{}'::TEXT[];
        RETURN NEXT;
        RETURN;
    END IF;

    -- Tenant status gate (design/01 §5.2), BEFORE the read_scopes build.
    SELECT status INTO v_status FROM context_tenants WHERE id = v_tenant_id;
    IF v_status IS NULL OR v_status <> 'active' THEN
        api_key_id     := NULL;
        home_scope     := '__UNAUTHORIZED__';
        allowed_scopes := '{}'::TEXT[];
        read_scopes    := '{}'::TEXT[];
        is_valid       := false;
        is_admin       := false;
        tenant_id      := NULL;
        tenant_role    := '';
        write_scopes   := '{}'::TEXT[];
        RETURN NEXT;
        RETURN;
    END IF;

    -- read_scopes POSITIONAL (design/02 §4.1 amendment, Variante A). Unchanged.
    v_read_scopes := ARRAY[v_home_scope::TEXT];
    v_cand := COALESCE(v_allowed_scopes, '{}'::TEXT[])
           || COALESCE((SELECT array_agg(g.granted_scope ORDER BY g.granted_scope)
                          FROM context_tenant_grants g
                         WHERE g.grantee_tenant = v_tenant_id), '{}'::TEXT[]);
    FOREACH v_s IN ARRAY v_cand LOOP
        IF v_s NOT LIKE '\_%' AND NOT (v_s = ANY(v_read_scopes)) THEN
            v_read_scopes := v_read_scopes || v_s;
        END IF;
    END LOOP;

    -- Valid key (+1 new column). write_scopes is returned RAW (COALESCE floor to
    -- '{}' for a NULL column); the ⊆ (allowed ∪ home) intersection is a Go-side
    -- concern (writableBlockScopes), NOT re-implemented here — the DB stays the
    -- record of intent, the gate stays the single fail-closed eval point.
    api_key_id     := v_api_key_id;
    home_scope     := v_home_scope;
    allowed_scopes := v_allowed_scopes;
    read_scopes    := COALESCE(NULLIF(v_read_scopes, '{}'::TEXT[]), ARRAY[v_home_scope]::TEXT[]);
    is_valid       := true;
    is_admin       := v_is_admin;
    tenant_id      := v_tenant_id;
    tenant_role    := v_tenant_role;
    write_scopes   := COALESCE(v_write_scopes, '{}'::TEXT[]);
    RETURN NEXT;
    RETURN;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (78, '078_write_scopes.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 078_write_scopes.sql

-- @@ ctx-fold begin 079_context_projects.sql
-- =============================================================================
-- 079_context_projects.sql — project registry + sync run history (workflow W4)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- One row = one project = one repo corpus. scope is the data discriminator
-- (Modell C): every issue/comment block of the project lives in this scope;
-- isolation, RRF, Guard, Dream come for free (vision 019e83df-9666).
-- Design: design/03-workflow-api-cli.md §3.1 (provisional M072, FINAL number 079
-- per masterplan §2 K1); ownership K14 (03-W4 owns the schema; 02-I-F extends it
-- ADDITIVELY in 080 — sync_cursor/forge columns are already the Achse-02 contract).
--
-- INVARIANT: tenant_id = tenant_of(scope). Both columns exist (FK-fast joins),
-- but tenant_id is DERIVED from context_tenant_scopes inside the create-Tx —
-- never taken from the request — and PATCH never changes scope (one source of
-- truth, no inconsistent pairs). The create-Tx assigns the scope to the binding
-- tenant and stamps tenant_id with that same tenant, so the pair is consistent
-- by construction (store.CreateProject).
--
-- identity survives clones/moves: 'github:owner/repo' | 'git-root:<sha>' |
-- 'manual:<slug>' — validated in Go (v2.0.0 line: no CHECK on open sets).
--
-- webhook_secret_ref names a context_secrets row IN THE PROJECT SCOPE with the
-- SERVER-FIXED name 'webhook.github.<project_id>' (§5.3/§5.6). The column is
-- server-managed (set by the W13 webhook-secret lifecycle endpoint, NEVER by
-- PATCH — design/03 §4.2); the plaintext HMAC secret never touches this table.
-- In W4 it is always NULL (no webhook surface yet); the column is fixed now so
-- no later wave rewrites the table.
--
-- lock_timeout (R-MIG2): CREATE TABLE / CREATE INDEX on empty new tables take
-- only brief catalog locks. Forward-only, additive: no backfill (new surface,
-- no existing data), existing scopes/blocks stay byte-identical.
-- Idempotent: CREATE TABLE/INDEX IF NOT EXISTS re-run cleanly; _migrations
-- INSERT ON CONFLICT (version) DO NOTHING.
-- test.sh T07 table count: +2 tables (context_projects, context_project_sync_runs)
-- → 31 → 33; context_blocks columns UNCHANGED (39).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_projects (
    id                 UUID PRIMARY KEY DEFAULT uuidv7(),
    tenant_id          UUID NOT NULL REFERENCES context_tenants(id),
    scope              VARCHAR(50) NOT NULL REFERENCES context_tenant_scopes(scope),
    identity           TEXT NOT NULL,                  -- 'github:owner/repo' | 'git-root:<sha>' | 'manual:<slug>'
    display_name       TEXT NOT NULL DEFAULT '',
    forge              JSONB NOT NULL DEFAULT '{}',    -- {kind:'github', owner, repo, api_base?} — Forge-Abstraktion (Achse 02); api_base-Mutation: §5.7/E6
    webhook_secret_ref TEXT,                           -- server-fixed 'webhook.github.<project_id>' in the PROJECT scope; NULL = no webhook (W13)
    sync_status        TEXT NOT NULL DEFAULT 'idle',   -- idle|running|error — display copy; truth = run-state (§4.4)
    last_sync_at       TIMESTAMPTZ,
    sync_cursor        JSONB NOT NULL DEFAULT '{}',    -- forge-side progress (ETag/updated-since; Achse-02 contract)
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by         UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,
    metadata           JSONB NOT NULL DEFAULT '{}',
    CONSTRAINT uq_projects_scope    UNIQUE (scope),               -- 1 project : 1 scope
    CONSTRAINT uq_projects_identity UNIQUE (tenant_id, identity)  -- Re-Init = idempotency, no duplicate
);
CREATE INDEX IF NOT EXISTS idx_projects_tenant ON context_projects (tenant_id);

-- Sync run history: the counting substrate for project.sync.rate_limit (§4.4 —
-- the I6 mechanic CANNOT carry it: hardcoded 60-s window, keyed per api_key_id,
-- store/blocks.go:277-291; context_access_log has no project dimension). One row
-- per started run; doubles as diagnosis history for `ctx project issues sync
-- --status`. ON DELETE CASCADE off context_projects so a project-delete (or the
-- K14 tenant prune) drains the run history for free — no extra prune step.
CREATE TABLE IF NOT EXISTS context_project_sync_runs (
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    project_id  UUID NOT NULL REFERENCES context_projects(id) ON DELETE CASCADE,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    status      TEXT NOT NULL DEFAULT 'running',  -- running|done|error|interrupted
    error       TEXT,
    stats       JSONB NOT NULL DEFAULT '{}'       -- Achse-02 contract: fetched/upserted/conflicts
);
CREATE INDEX IF NOT EXISTS idx_sync_runs_project
    ON context_project_sync_runs (project_id, started_at DESC);

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (79, '079_context_projects.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 079_context_projects.sql

-- @@ ctx-fold begin 080_forge_sync.sql
-- =============================================================================
-- 080_forge_sync.sql — forge sync-state extension + issue↔block mapping (I-F)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Workflow-engine axis 02, Welle I-F (design/02-issue-workflow.md §3.4/§3.5,
-- §4.5). ADDITIVE extension of the project register (079, W4) — masterplan K14:
-- there is NO separate context_forge_repos table; context_projects (079) IS the
-- forge registration (identity, scope binding, forge JSONB, sync_status/
-- last_sync_at/sync_cursor already shipped in 079). This migration only adds the
-- sync-state columns 079 did not carry, plus the per-entity mapping table.
--
-- ── K14 TRANSLATION of design/02 §3.4 (documented deviation) ──────────────────
-- design/02 §3.4 sketches a standalone `context_forge_repos` (owner/repo/etag_*/
-- since_*/local_seq/…). K14 collapses that onto context_projects. The mapping:
--   context_forge_repos.identity/owner/repo/scope/forge_kind → context_projects
--       (identity + forge JSONB {kind,owner,repo,api_base?}, already in 079)
--   context_forge_repos.token_secret       → context_projects.token_secret (HERE)
--   context_forge_repos.sync_enabled       → context_projects.sync_enabled (HERE)
--   context_forge_repos.push_enabled       → context_projects.push_enabled (HERE)
--   context_forge_repos.last_error         → context_projects.last_error   (HERE)
--   context_forge_repos.backoff_until      → context_projects.backoff_until(HERE)
--   context_forge_repos.etag_*/since_*     → context_projects.sync_cursor JSONB
--       (079 already reserves sync_cursor as "forge-side progress (ETag/updated-
--        since); Achse-02 contract" — NO new columns, the JSONB carries them)
--   context_forge_repos.local_seq          → DEFERRED to I-H (draft #L<n> push
--        numbering; pull-only I-F has no draft-number surface)
-- The mapping table `context_forge_sync` (§3.5) becomes context_project_sync_map,
-- keyed on project_id → context_projects (was repo_id → context_forge_repos).
--
-- ── OWNERSHIP: what I-F writes vs. I-G ────────────────────────────────────────
-- The mapping table's block_id is NOT NULL and references a real block. Block
-- creation + the 3-way hash + mapping-row writes are the Pull-APPLY step, which
-- design/02 §7 assigns to Welle I-G ("Pull-Apply (Blocks/Comments/Status/links)").
-- I-F creates the TABLE (schema) and the client/sync-shell that fetches; it does
-- NOT write mapping rows (no block_id exists without apply). base_hash/conflict/
-- conflict_at/forge_updated_at are the I-G 3-way columns, shipped here forward-
-- only so I-G needs no further migration.
--
-- ── PRUNE (K14) ───────────────────────────────────────────────────────────────
-- context_project_sync_map CASCADEs off BOTH context_projects (project_id) and
-- context_blocks (block_id). PruneTenant drains the block corpus (scope-batched)
-- then context_projects (tenant-keyed) — the mapping rows cascade for free from
-- either side, exactly like context_project_sync_runs (079). No new drain step;
-- the store/tenant.go PruneTenant comment is extended to record this, and the
-- I-F gate proves "PruneTenant ⇒ 0 mapping rows of the tenant".
--
-- ── LOCKS / IDEMPOTENCY (R-MIG2, 069 pattern) ─────────────────────────────────
-- ADD COLUMN IF NOT EXISTS with a constant/NULL DEFAULT is a metadata-only
-- catalog change on PG18 (no table rewrite); CREATE TABLE/INDEX IF NOT EXISTS
-- take brief catalog locks on an empty new table. Forward-only, no backfill
-- (new surface), existing rows/blocks byte-identical. Idempotent on re-run.
-- test.sh T07 table count: +1 table (context_project_sync_map) → 33 → 34;
-- context_blocks columns UNCHANGED (40). context_projects columns +5.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- 1: forge sync-state columns on the project register (K14 additive extension).
--    token_secret  = NAME of a context_secrets row in the PROJECT scope (server-
--                    fixed 'forge.token.<project_id>'); the PAT plaintext NEVER
--                    lives here (sealbox line, 051). NULL = local-only / unauth.
--    sync_enabled  = the periodic loop's iterate predicate (fail-closed: a scope
--                    that lost its tenant is set false by the run, §4.5.5/S13).
--    push_enabled  = fail-closed pull-only until an explicit tenant-admin toggle
--                    (§5.6; the write channel opens in I-H, default false now).
--    last_error / backoff_until = offline-first resilience: a wire/rate-limit
--                    error stamps these and the run backs off exponentially
--                    (cap 1h), local work continues (§4.5.3).
ALTER TABLE context_projects
    ADD COLUMN IF NOT EXISTS token_secret  TEXT,
    ADD COLUMN IF NOT EXISTS sync_enabled  BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS push_enabled  BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS last_error    TEXT,
    ADD COLUMN IF NOT EXISTS backoff_until TIMESTAMPTZ;

-- 2: per-entity issue↔block mapping + 3-way sync state (design/02 §3.5/§3.6).
--    Sync writes NEVER go through UpsertBlock (§3.5): the sync identity is
--    (project, kind, forge_id) → block_id, so forge-side title changes are
--    identity-neutral and the 3-way base_hash lives on the mapping row, not the
--    block. forge_id = GitHub issue number / comment id; 0 = local-only (created
--    in ctx, not yet pushed — I-H). base_hash = sha256 of the canonical
--    projection at last successful sync (§3.6; WRITTEN by I-G apply, never here).
--    forge_updated_at is telemetry/display ONLY — never a direction input (W16).
CREATE TABLE IF NOT EXISTS context_project_sync_map (
    project_id       UUID NOT NULL REFERENCES context_projects(id) ON DELETE CASCADE,
    entity_kind      TEXT NOT NULL,                    -- 'issue' | 'comment'
    forge_id         BIGINT NOT NULL DEFAULT 0,        -- GitHub number/id; 0 = local-only (I-H push)
    block_id         UUID NOT NULL REFERENCES context_blocks(id) ON DELETE CASCADE,
    base_hash        VARCHAR(64) NOT NULL,             -- sha256 canonical projection @ last sync (I-G writes)
    conflict         BOOLEAN NOT NULL DEFAULT false,   -- 3-way divergence (I-G sets)
    conflict_at      TIMESTAMPTZ,
    forge_updated_at TIMESTAMPTZ,                      -- telemetry ONLY, never direction (W16)
    synced_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata         JSONB NOT NULL DEFAULT '{}',
    PRIMARY KEY (project_id, entity_kind, forge_id, block_id)
);

-- One mapping row per block (a block belongs to at most one forge entity).
CREATE UNIQUE INDEX IF NOT EXISTS uq_sync_map_block
    ON context_project_sync_map (block_id);
-- Conflict surface for forge-sync-status/CLI/UI: partial, small (§6.3).
CREATE INDEX IF NOT EXISTS idx_sync_map_conflict
    ON context_project_sync_map (project_id) WHERE conflict;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (80, '080_forge_sync.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 080_forge_sync.sql

-- @@ ctx-fold begin 081_project_notify.sql
-- =============================================================================
-- 081_project_notify.sql — project-scoped NOTIFY for the SSE domain-event hub (W9)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Workflow-Achse 03, Welle W9 (design/03-workflow-api-cli.md §4.5/§6.2, §7-W9).
-- Feeds the projectHub → GET /api/project/events per-scope SSE fanout. K1 pins
-- this to migration 081 (the design's provisional "073" is stale; 082=W13).
--
-- ── VEHICLE DECISION (T6-Befund: NOTIFY O(n²) at bulk-Tx) ─────────────────────
-- The design sketch (§6.2) reuses the existing ctx_block_write NOTIFY (004/051,
-- row-level AFTER INSERT/UPDATE, payload {id,op}) and coalesces at the hub. Two
-- reasons this migration builds a DEDICATED channel + dedicated triggers instead
-- of extending the guard/digest trigger:
--
--   1. T6-Befund — Postgres' PreCommit_Notify de-dups the pending-notify list in
--      O(n²) of the notifies queued IN ONE TRANSACTION. The forge Pull-APPLY path
--      commits PER ROW (forge/apply.go: pullCreate/pullUpdate each open their own
--      tx), so a 10k-import is 10k single-notify txs → NO O(n²). But PruneTenant
--      (store/tenant.go) mass-DELETEs context_blocks in 2000-row BATCHES via one
--      pool.Exec each = one implicit tx of 2000 row-notifies → O(2000²) per batch.
--      A naive row-level DELETE notify would light exactly that storm. So DELETE
--      here is STATEMENT-LEVEL with a transition table: ONE trigger fire per
--      statement, coalesced to O(distinct scopes) notifies — the batch storm is
--      structurally impossible.
--   2. Blast radius — extending notify_block_write() would fire the project
--      channel for the ENTIRE knowledge corpus (every block write) and couple the
--      guard/digest listener to the hub. A dedicated channel with a WHEN
--      type-filter fires ONLY for issue/comment rows, and leaves the guard/digest
--      trigger byte-for-byte untouched (zero regression surface).
--
-- INSERT/UPDATE stay ROW-LEVEL: they carry the block id for the id-level frame
-- ({project_id, block_ids, kind, op}, §4.5), and no bulk single-tx issue/comment
-- INSERT/UPDATE path exists today (apply is per-row-tx; API writes are single).
-- If a future wave adds a bulk single-tx issue write, revisit (per-scope
-- statement-level, or per-batch commit) — documented scope boundary (W21).
--
-- ── LISTENER-DISCARD (old binary against the new trigger) ─────────────────────
-- A NOTIFY with no LISTENer is a no-op in Postgres. An old binary does not LISTEN
-- ctx_project_write, so every notify this migration fires is silently discarded —
-- the old binary runs byte-for-byte unchanged (the pausability/rollback
-- invariant, 078 line). The new binary's projectHub is the only consumer.
--
-- ── FRAME PAYLOAD (ids-only, never content — K16) ─────────────────────────────
-- Payload = {id, op, scope, type} (INSERT/UPDATE) or {op:'DELETE', scope, type,
-- bulk:true} (statement-level DELETE). NEVER title/content/body — the hub fans
-- out ids only and the client refetches over the read API, so there is no
-- content-leak path through the stream. NOTIFY payload budget (8000 bytes) is
-- undercut by orders of magnitude (one UUID + three short strings).
--
-- ── LOCKS / IDEMPOTENCY (R-MIG2, 065 pattern) ─────────────────────────────────
-- Function-only + trigger create: brief catalog locks, no table/column change.
-- CREATE OR REPLACE FUNCTION + DROP TRIGGER IF EXISTS + CREATE TRIGGER re-run
-- cleanly; _migrations INSERT ON CONFLICT (version) DO NOTHING. Forward-only, no
-- backfill (no data touched). test.sh T07 table count UNCHANGED (no new table).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- Row-level INSERT/UPDATE: one notify per issue/comment write, carrying the block
-- id (id-level frame). The WHEN clause keeps the whole non-workflow corpus from
-- firing this at all. op='UPDATE' also covers archive-"deletes" (is_archived
-- UPDATE): the client refetches and sees is_archived (§4.5, I17).
CREATE OR REPLACE FUNCTION notify_project_write() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('ctx_project_write', json_build_object(
        'id',    NEW.id,
        'op',    TG_OP,
        'scope', NEW.scope,
        'type',  NEW.type_name)::text);
    RETURN NULL; -- AFTER trigger: return value ignored
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_project_write ON context_blocks;
CREATE TRIGGER trg_project_write
    AFTER INSERT OR UPDATE ON context_blocks
    FOR EACH ROW
    WHEN (NEW.type_name IN ('issue', 'comment'))
    EXECUTE FUNCTION notify_project_write();

-- Statement-level DELETE (physical prune, §3.2): ONE fire per DELETE statement,
-- transition table aggregated to DISTINCT scope — O(distinct scopes) notifies per
-- prune batch instead of O(rows) (T6-Befund defused). bulk:true tells the hub to
-- emit a refetch/removal frame (no ids: a prune drops whole ranges). A prune of a
-- tenant's scopes usually reaches no live subscriber (the tenant's keys are gone
-- → re-auth ends their streams), so this exists mainly to keep the batch from
-- lighting the O(n²) storm, not for a feature.
CREATE OR REPLACE FUNCTION notify_project_delete() RETURNS trigger AS $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN
        SELECT DISTINCT scope, type_name
          FROM oldrows
         WHERE type_name IN ('issue', 'comment')
    LOOP
        PERFORM pg_notify('ctx_project_write', json_build_object(
            'op',    'DELETE',
            'scope', r.scope,
            'type',  r.type_name,
            'bulk',  true)::text);
    END LOOP;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_project_delete ON context_blocks;
CREATE TRIGGER trg_project_delete
    AFTER DELETE ON context_blocks
    REFERENCING OLD TABLE AS oldrows
    FOR EACH STATEMENT
    EXECUTE FUNCTION notify_project_delete();

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (81, '081_project_notify.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 081_project_notify.sql

-- @@ ctx-fold begin 082_webhook_inbox.sql
-- =============================================================================
-- 082_webhook_inbox.sql — inbound forge event queue (workflow W13)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- One row = one inbound webhook delivery. The table is a DURABLE, DEBOUNCE-able
-- TRIGGER queue, NOT an authority store: the scheduler inbox arm drains pending
-- rows and fires a forge SyncManager pull per project — the payload is never
-- upserted into a block (design/03-workflow-api-cli.md §5.3: "Events sind Sync-
-- TRIGGER, nie Autoritätsquelle"; the 3-way content hash against the forge IST-
-- state lives in the Achse-02 translator). The audit of the resulting block
-- writes lives in context_write_log, not here — this table is a through-queue.
--
-- Design: design/03 §3.4 (provisional M075, FINAL number 082 per masterplan K1 —
-- 081 project-notify landed at W9, 082 was reserved for W13). Schema/route-shape/
-- security model were fixed at W4 (§3.4/§5.3/§5.6) so no earlier wave rewrites it.
--
-- Redelivery-idempotency (NOT replay-protection, §5.3): UNIQUE(project_id,
-- delivery_id) + INSERT … ON CONFLICT DO NOTHING makes GitHub's own redeliveries
-- (identical X-GitHub-Delivery GUID) collapse to exactly ONE row. The GUID is an
-- UNSIGNED header, so this is no defense against an active replayer — that
-- mitigation is the translator's 3-way hash / updated_at-cursor discard.
--
-- FK ON DELETE CASCADE off context_projects: a project-delete (or the K14 tenant
-- prune, which deletes context_projects rows) drains the inbox for free. The
-- per-project WEBHOOK SECRET is a separate context_secrets row and is drained by
-- store.DeleteProject (project delete) and store.PruneTenant (tenant prune) —
-- neither cascades from this table.
--
-- lock_timeout (R-MIG2): CREATE TABLE / CREATE INDEX on an empty new table take
-- only brief catalog locks. Forward-only, additive: no backfill (new surface).
CREATE TABLE IF NOT EXISTS context_webhook_events (
    id           UUID PRIMARY KEY DEFAULT uuidv7(),
    project_id   UUID NOT NULL REFERENCES context_projects(id) ON DELETE CASCADE,
    delivery_id  TEXT NOT NULL,                    -- X-GitHub-Delivery (GUID, unsigned header)
    event        TEXT NOT NULL,                    -- X-GitHub-Event ('issues','issue_comment',…)
    payload      JSONB NOT NULL,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at TIMESTAMPTZ,
    status       TEXT NOT NULL DEFAULT 'pending',  -- pending|done|error|skipped
    error        TEXT,
    CONSTRAINT uq_webhook_delivery UNIQUE (project_id, delivery_id)  -- redelivery-idempotent (§5.3)
);

-- Queue predicate: the scheduler inbox arm picks processed_at IS NULL with
-- FOR UPDATE SKIP LOCKED (the embed-backfill pattern). Partial index so the scan
-- never walks the processed history.
CREATE INDEX IF NOT EXISTS idx_webhook_pending
    ON context_webhook_events (received_at) WHERE processed_at IS NULL;

-- Counting window for webhook.rate_limit (§4.4/§5.3): the inbound path is
-- UNAUTHENTICATED (no api_key_id), so the I6 write-throttle mechanic is
-- structurally unusable. count(*) WHERE project_id=$1 AND received_at > now()-'60s'
-- rides this index. Doubles as per-project diagnosis (recent deliveries first).
CREATE INDEX IF NOT EXISTS idx_webhook_project_recent
    ON context_webhook_events (project_id, received_at DESC);

-- Retention-eviction path: the Janitor arm evicts received_at < now()-interval
-- AND processed_at IS NOT NULL (Config webhook.retention, default 14d). This
-- partial index is the exact COMPLEMENT of idx_webhook_pending so the eviction
-- DELETE is index-driven, never a Seq-Scan over 120/min × 14d × N-project rows.
CREATE INDEX IF NOT EXISTS idx_webhook_done
    ON context_webhook_events (received_at) WHERE processed_at IS NOT NULL;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (82, '082_webhook_inbox.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 082_webhook_inbox.sql

-- @@ ctx-fold begin 083_block_type_refcount_index.sql
-- =============================================================================
-- 083_block_type_refcount_index.sql — index the block-type reference count.
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Workflow-engine axis 03, wave W2 (design/03 §4.2 DELETE row / §6 target
-- scale). W2 exposes DELETE /api/types/{name}; its reference guard
-- (store.DeleteBlockType) runs
--
--     SELECT count(*) FILTER (WHERE NOT is_archived),
--            count(*) FILTER (WHERE is_archived)
--       FROM context_blocks WHERE type_name = $1
--
-- to turn a still-referenced type into a 409 + count. context_blocks.type_name
-- (renamed from block_role in 071) carries NO index: idx_block_type has indexed
-- lifecycle_state since 070 renamed the OLD block_type column out from under it.
-- At the 1M+-blocks/tenant target scale an unindexed WHERE type_name = $1 is a
-- seq-scan on every type delete, so the guard would degrade linearly with the
-- corpus. A plain btree on type_name makes it index-supported.
--
-- Plain CREATE INDEX (not CONCURRENTLY): the migration runner wraps every file
-- in ONE transaction (store/migrations.go), where CONCURRENTLY is illegal — the
-- established convention for every index migration in this tree (001/002/022).
-- The build takes a brief ACCESS SHARE-blocking lock on context_blocks; at the
-- current corpus it is instant, at target scale it is a maintenance-window op.
-- Forward-only, idempotent, self-registering (069 pattern).
-- =============================================================================

CREATE INDEX IF NOT EXISTS idx_blocks_type_name ON context_blocks(type_name);

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (83, '083_block_type_refcount_index.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 083_block_type_refcount_index.sql

-- @@ ctx-fold begin 084_issue_comment_type_seeds.sql
-- =============================================================================
-- 084_issue_comment_type_seeds.sql — issue/comment block-type seeds
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Workflow-engine axis 02, Welle I-C (design/02-issue-workflow.md §4.1). Two
-- builtin type-config rows registered in the context_block_types registry
-- (migration 072). Behaviour stays code (RRF, guard, dream, digest, overview);
-- HOW an issue/comment is treated is this DATA. The NOTIFY + audit triggers and
-- the table itself come from 072 — this migration only INSERTs rows.
--
-- The configs MUST decode byte-equivalently to the compiled-in builtin set in
-- internal/blocktype/builtin.go (issue/comment entries): the golden integration
-- test applies THIS file from migrations.FS and diffs the decoded rows against
-- the builtin set (drift gate, design/01 §4.1 R1). ON CONFLICT DO NOTHING keeps
-- the seed idempotent and never overwrites operator tuning on re-run.
--
-- ── issue ────────────────────────────────────────────────────────────────────
-- retrieval full-pass; guard participates in FLAG mode (a duplicate issue is
-- surfaced via a possible_duplicate flag, NEVER auto-archived, §4.7) restricted
-- to its own scope (guard.candidates=same-scope — an issue never matches a
-- cross-tenant block), with per-type thresholds 0.97/0.90; dream links issues.
-- digest.include=false AND overview.include=false: at 10k+ issues/repo the
-- topic-map (digest) and the Louvain overview clustering would drown otherwise
-- (§6.8 — the LOOP overview gate). workflow is the backlog→in-progress→done
-- state machine with the forge open/closed mapping (§4.2). structural_link_
-- classes = the write allowlist for context_structural_links edges of issues.
--
-- ── comment (INTERIM at I-C, FLIPPED by migration 085 at I-E) ─────────────────
-- Kept out of every autonomous pipeline: guard.check=false, guard.candidate=
-- false, dream.linkable=false, digest.include=false, overview.include=false
-- (all exact §4.1). At I-C this row DEVIATED from §4.1 in TWO fields, because
-- their mechanisms did not ship yet and the strict decoder rejects them fail-
-- closed:
--   * §4.1 wants retrieval=aggregate-to-parent, but the fold mechanism (Achse-01
--     T11) had NO consumer (Set.AggregateTypes unused) — accepting it would have
--     let comments leak raw into results. Interim: retrieval=excluded (the safe
--     subset: comment invisible, never leaked).
--   * §4.1 wants parent.mode=required + relationship=comment-of, but the
--     parent_id WRITE path (store.PutBlockParent) had no production caller (its
--     consumer is I-D's InsertCommentBlock) — required would have been silently
--     ineffective (§5.2). Interim: parent.mode=none.
-- RESOLVED: Welle I-E ships migration 085, which UPDATEs THIS row (in the same
-- lockstep as builtin.go) to the §4.1 target retrieval=aggregate-to-parent +
-- parent.mode=required/comment-of, now that T11 (fold) and I-D (parent_id write)
-- are both live. 084 stays the INTERIM seed (ON CONFLICT DO NOTHING, no rewrite);
-- 085 is the deliberate correcting UPDATE. Handoff recorded in design §9.1a.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config) VALUES
('issue', '_global', 'Issue', true, false, '{
  "v": 1,
  "retrieval": {"policy": "full-pass"},
  "guard":     {"check": true, "candidate": true, "mode": "flag", "candidates": "same-scope",
                "threshold_duplicate": 0.97, "threshold_review": 0.90},
  "dream":     {"linkable": true},
  "digest":    {"include": false},
  "overview":  {"include": false},
  "parent":    {"mode": "none"},
  "workflow":  {"states": ["backlog", "in-progress", "done"], "initial": "backlog",
                "terminal": ["done"], "forge_state_map": {"open": "backlog", "closed": "done"}},
  "structural_link_classes": ["references", "duplicate-of"],
  "classify":  {}
}'::jsonb),
('comment', '_global', 'Comment', true, false, '{
  "v": 1,
  "retrieval": {"policy": "excluded"},
  "guard":     {"check": false, "candidate": false},
  "dream":     {"linkable": false},
  "digest":    {"include": false},
  "overview":  {"include": false},
  "parent":    {"mode": "none"},
  "classify":  {}
}'::jsonb)
ON CONFLICT (name, scope) DO NOTHING;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (84, '084_issue_comment_type_seeds.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 084_issue_comment_type_seeds.sql

-- @@ ctx-fold begin 085_comment_seed_flip.sql
-- =============================================================================
-- 085_comment_seed_flip.sql — comment type: INTERIM → §4.1 target (Achse 02, I-E)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle I-E (design/02-issue-workflow.md §4.1 / §4.4). Migration 084 seeded the
-- comment type in an INTERIM shape (retrieval=excluded, parent.mode=none) because
-- BOTH mechanisms the §4.1 target needs were unbuilt at I-C time:
--   * the aggregate-to-parent FOLD consumer (Set.AggregateTypes → QueryHandler.
--     foldAggregates) — shipped by Achse-01 T11;
--   * the parent_id WRITE path (store.PutBlockParent, store.InsertCommentBlock) —
--     shipped by Achse-02 I-D.
-- Both are now live (base HEAD T11 + I-D). The decoder's cross-field rule
-- (policy.go: aggregate-to-parent REQUIRES parent.mode != none) and the parent.
-- mode gate both accept the target — the positive probe is
-- blocktype/policy_test.go::TestCommentSeedConfigTarget. So I-E flips the seed to
-- the §4.1 values in lockstep with internal/blocktype/builtin.go (the drift gate
-- registry_integration_test.go::TestRegistryGolden_Integration diffs the decoded
-- DB row against the compiled-in builtin set — both move together or it goes red).
--
-- retrieval=aggregate-to-parent: a comment that ranks in RRF is NOT delivered as
-- itself — it folds onto its parent issue (parent_id), carrying the issue identity
-- + a matched_comment annotation (§4.4). This makes comment a VISIBLE retrieval
-- type (Set.VisibleTypes), unlike the interim excluded shape.
-- parent.mode=required + relationship=comment-of: a comment is never created
-- orphaned (InsertCommentBlock mandates a parent); the fold keeps a parent_id=NULL
-- WARN as the defensive read-side line only.
-- The autonomous-pipeline fields stay OFF (guard.check=false, dream.linkable=
-- false, digest.include=false, overview.include=false) — unchanged from 084.
--
-- This is a deliberate UPDATE of the builtin _global comment row (not ON CONFLICT
-- DO NOTHING like a seed): the flip is a correction of the row 084 planted, and
-- the golden gate requires DB == builtin.go. Tenant-scoped overrides shadow the
-- _global row via a separate row and are untouched. Idempotent (fixed target
-- values); a second run is a no-op write.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

UPDATE context_block_types
SET config = '{
  "v": 1,
  "retrieval": {"policy": "aggregate-to-parent"},
  "guard":     {"check": false, "candidate": false},
  "dream":     {"linkable": false},
  "digest":    {"include": false},
  "overview":  {"include": false},
  "parent":    {"mode": "required", "relationship": "comment-of"},
  "classify":  {}
}'::jsonb
WHERE name = 'comment' AND scope = '_global' AND builtin = true;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (85, '085_comment_seed_flip.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 085_comment_seed_flip.sql

-- @@ ctx-fold begin 086_workflow_created_index.sql
-- =============================================================================
-- 086_workflow_created_index.sql — immutable-keyset board index for ?sort=created
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- Workflow-engine axis 03, Welle W6 (design/03-workflow-api-cli.md §6.1).
-- =============================================================================
-- The board index idx_blocks_workflow_board (M077) orders on updated_at DESC —
-- the UI board view. updated_at is MUTABLE: a row updated mid-pagination moves
-- ahead of the consumed cursor and drops out of the traversal (documented list
-- semantics, §6.1). For the LOSSLESS traversal that agents/export need, W6 offers
-- ?sort=created — keyset on the IMMUTABLE (created_at, id). That ordering needs
-- its own index: the board index cannot serve a created_at-ordered range scan
-- (created_at is not one of its keys), so an ORDER BY created_at over the scope
-- would force a Sort node (the exact RED the board index avoids for updated_at).
--
-- K4 NOTE (design/03 §6.1, masterplan K4): the q-filter was decided to bind to
-- the EXISTING FTS tsvector GIN path (idx_context_ts_de / idx_context_ts_en,
-- M001/M044) — no trigram migration is needed (idx_trgm_title already exists too,
-- M001). This freed the W6 migration slot (086) for the created-sort index below.
--
-- Ordering + partiality mirror the board index (M077) exactly so the same keyset
-- machinery (tuple comparison against the all-DESC index direction) applies:
--   - all-DESC (created_at DESC, id DESC) ⇒ (created_at, id) < (cur) is a clean
--     ordered range scan, no Sort, no bitmap.
--   - workflow_status is NOT an index key (unlike the board index): created_at is
--     monotone across ALL statuses, so ONE range scan serves both the
--     status-filtered AND the status-unfiltered created traversal WITHOUT a
--     per-status merge — the architectural simplification created-sort buys over
--     the updated board path (§6.1).
--   - partial WHERE workflow_status IS NOT NULL AND NOT is_archived keeps it lean
--     (only live workflow rows), same as M077; a created-sort query MUST carry the
--     `workflow_status IS NOT NULL` predicate to match this partial index.
--
-- INDEX NOTE (063/069/077 house norm): plain CREATE INDEX in the single-Tx runner
-- (CONCURRENTLY forbidden) holds a SHARE lock for the build. Fine at today's
-- scale; at 1M+ rows build it OUT-OF-BAND first with
--   CREATE INDEX CONCURRENTLY idx_blocks_workflow_created ...
-- and the IF NOT EXISTS below then finds it (this migration becomes a no-op).
--
-- Backfill: none — additive index, no data change (pausability invariant).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE INDEX IF NOT EXISTS idx_blocks_workflow_created
    ON context_blocks (scope, type_name, created_at DESC, id DESC)
    WHERE workflow_status IS NOT NULL AND NOT is_archived;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (86, '086_workflow_created_index.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 086_workflow_created_index.sql

-- @@ ctx-fold begin 087_member_scope.sql
-- =============================================================================
-- 087_member_scope.sql — denormalized scope on graph_cluster_member (B-W2)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Per-tenant overview line (overnight plan B): the rebuild loop (B-W6) tears
-- down and re-aggregates ONE tenant's partition per run. The scope-scoped
-- DELETE and the scope-scoped aggregation (B-W3) both need the member's scope
-- without joining context_blocks mid-teardown — so the member row carries it,
-- denormalized from the Louvain INPUT at insert time (persist writes what it
-- clustered, never a re-read: a concurrent block scope-move between load and
-- persist must not make the member row disagree with the partition it was
-- computed in).
--
-- Column type is TEXT — the 057 family convention (graph_cluster_node.scope,
-- graph_cluster_edge.scope_s/scope_t are TEXT; context_blocks.scope stays the
-- VARCHAR(50) source of truth, the join is type-compatible).
--
-- PK DECISION (lead review, load-bearing invariant): block_id remains the
-- SOLE primary key (057). That is correct exactly as long as the overview
-- input is strictly owned-disjoint — no grants in the Louvain input, so no
-- block can be a member under two scopes. B-W6 adds the input-purity
-- assertion; until then this header and the persist comment document the
-- invariant.
--
-- Backfill: existing rows get their block's current scope (FK ON DELETE
-- CASCADE guarantees every member has a live block, context_blocks.scope is
-- NOT NULL — so the backfill leaves no NULLs and SET NOT NULL is safe).
--
-- ROLLBACK NOTE (deliberate, 089/090 line): an old binary whose member INSERT
-- does not carry scope fails the NOT NULL constraint — the rebuild tx rolls
-- back LOUDLY and the previous overview tables stay readable (advisory-locked
-- replace, no partial state). Fail-loud beats a silent wrong-scope default:
-- any DEFAULT here would poison the B-W3 scope-scoped aggregation.
--
-- lock_timeout (R-MIG2): ADD COLUMN (nullable) + UPDATE on a <100k-row table
-- + SET NOT NULL take brief locks; the runner wraps the migration in one tx.
-- Idempotent: ADD COLUMN IF NOT EXISTS; the backfill UPDATE only touches NULL
-- rows; SET NOT NULL re-runs cleanly; _migrations INSERT ON CONFLICT DO
-- NOTHING. Forward-only. Additive column → test.sh table count UNCHANGED.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE graph_cluster_member
    ADD COLUMN IF NOT EXISTS scope TEXT;

UPDATE graph_cluster_member m
   SET scope = b.scope
  FROM context_blocks b
 WHERE b.id = m.block_id
   AND m.scope IS NULL;

ALTER TABLE graph_cluster_member
    ALTER COLUMN scope SET NOT NULL;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (87, '087_member_scope.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 087_member_scope.sql

-- @@ ctx-fold begin 088_meta_scope_pk.sql
-- =============================================================================
-- 088_meta_scope_pk.sql — graph_overview_meta singleton → scope PK (B-W5)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Per-tenant overview line (overnight plan B): the 057 meta table was a
-- single row ("last rebuild"); with per-partition rebuilds (B-W3/B-W6) each
-- scope carries its own computed_at — the read path answers "how fresh is MY
-- overview" as max(computed_at) over the caller's readScopes, never another
-- tenant's timestamp (leak B1-m1). Writer and reader change in the SAME wave
-- (masterplan B-W5: schema + read are non-divisible).
--
-- DATA MIGRATION (B3-M2): the existing singleton row is rewritten to one row
-- per REAL scope (DISTINCT scope FROM graph_cluster_node) with its
-- computed_at preserved — the boot-time rebuild check (overviewNeverBuilt =
-- zero rows) keeps its verdict: previously-built stays "built" (no spurious
-- boot rebuild), never-built stays empty. Degenerate case: a meta row exists
-- but graph_cluster_node is empty (meta without data) ⇒ zero rows after the
-- migration and the boot rebuild fires — correct there, it rebuilds what the
-- meta row only pretended to describe.
--
-- Sentinel note (B3-M1): rows carry REAL scopes only — no sentinel scope, no
-- computed_at:null window. The transition-phase writer (cluster.go, this
-- wave) derives its scope set from graph_cluster_node in the same tx.
--
-- Also here (B-W3 finding): graph_cluster_member(scope) gets an index — the
-- scoped teardown DELETE (and the scoped aggregation joins) would otherwise
-- seq-scan a 1M+ member table on every per-tenant rebuild.
--
-- edge_n semantics change with the per-scope rows: it counts INTRA-partition
-- edge rows (scope_s = scope_t = scope). Cross-scope rows from a pre-B global
-- run belong to no single partition and appear in no meta row; the global
-- node_n/cluster_n sums are recoverable as sum() over rows.
--
-- lock_timeout (R-MIG2): ALTER/DROP COLUMN, the data move (1 row → N scopes,
-- N = live scope count) and CREATE INDEX on the <100k member table take brief
-- locks; the runner wraps the whole file in one tx (atomic).
-- Idempotent: ADD/DROP COLUMN IF (NOT) EXISTS; the data-move INSERT only
-- fires while a scope-less row exists; SET NOT NULL and the guarded ADD
-- PRIMARY KEY re-run cleanly; CREATE INDEX IF NOT EXISTS; _migrations INSERT
-- ON CONFLICT DO NOTHING. Forward-only. No new table → test.sh count
-- UNCHANGED.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE graph_overview_meta
    ADD COLUMN IF NOT EXISTS scope TEXT;

-- Dropping the singleton column drops the old PK and its CHECK with it —
-- required BEFORE the data move (two singleton=true rows cannot coexist).
ALTER TABLE graph_overview_meta
    DROP COLUMN IF EXISTS singleton;

-- Data move (B3-M2): duplicate the scope-less legacy row onto every real
-- scope, preserving computed_at + stats; then retire the legacy row.
INSERT INTO graph_overview_meta (scope, computed_at, modularity, cluster_n, node_n, edge_n, resolution)
SELECT s.scope, m.computed_at, m.modularity, m.cluster_n, m.node_n, m.edge_n, m.resolution
  FROM graph_overview_meta m
 CROSS JOIN (SELECT DISTINCT scope FROM graph_cluster_node) AS s(scope)
 WHERE m.scope IS NULL
   AND NOT EXISTS (SELECT 1 FROM graph_overview_meta e WHERE e.scope = s.scope);

DELETE FROM graph_overview_meta WHERE scope IS NULL;

ALTER TABLE graph_overview_meta
    ALTER COLUMN scope SET NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'graph_overview_meta'::regclass AND contype = 'p'
    ) THEN
        ALTER TABLE graph_overview_meta ADD PRIMARY KEY (scope);
    END IF;
END $$;

-- B-W3 finding: the scoped teardown DELETE needs this at 1M+ member rows.
CREATE INDEX IF NOT EXISTS idx_gcm_scope ON graph_cluster_member (scope);

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (88, '088_meta_scope_pk.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 088_meta_scope_pk.sql

-- @@ ctx-fold begin 089_pending_writes.sql
-- =============================================================================
-- 089_pending_writes.sql — F6-C6 write-confirmation staging store (D-W1)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- One row = one STAGED write (MCP/Chat LLM path). The LLM client stages a
-- store/update, the server holds the authoritative payload, and only a confirm
-- call that atomically consumes the row executes the write. REST/CLI stay
-- direct (masterplan D-E1: gating is a per-principal distrust tool for LLM
-- harnesses, not a second layer under human paths).
--
-- TimescaleDB hypertable (masterplan D-E4, decision board 2026-07-05): eviction
-- is chunk-drop (D-W3 ticker), never row-DELETE — no dead-tuple bloat, no
-- Seq-Scan at 1M+ scale. Repo precedent: context_llm_log (025, 7-day chunks).
-- Chunk interval here is 1 HOUR: confirm_ttl is minute-scale (default 10m) and
-- confirm_retention hour-scale (default 24h), so 1h chunks keep the drop
-- granularity well below the retention window (24 chunks per day at rest —
-- trivial catalog load at the measured stage rate of well under 1 write/min,
-- D-W0 2026-07-05: 644 external writes in 30 days, peak minute 7 incl. the
-- non-gated dream pipeline).
--
-- TWO decoupled knobs (masterplan D-E3, fixes D2-C1's double-break):
--   writes.confirm_ttl       — expiry clock; expires_at = now()+ttl at stage
--                              time. ttl=0 ⇒ expires_at IS NULL (stage never
--                              expires; 0-is-off convention like llmlog/
--                              webchat/webhook retention). NOT wired to
--                              eviction.
--   writes.confirm_retention — D-W3 chunk-drop window (created_at-based).
--                              0 = keep forever. Independent of the expiry
--                              clock, so ttl=0 is NOT feature-death and
--                              retention=0 is NOT expiry-death.
--
-- UNIQUE constraint note (hypertable restriction): the draft's partial unique
-- index (api_key_id, payload_hash) WHERE consumed_at IS NULL cannot exist on a
-- hypertable — unique indexes must include the partitioning column, which
-- would defeat the dedup purpose. Stage idempotency is therefore APP-SIDE: a
-- re-arm CTE updates the open row's expiry, inserting only when no open row
-- matched (store.StagePendingWrite). A concurrent duplicate race leaves two
-- open rows with the SAME hash = the SAME server-held payload; consume picks
-- exactly one deterministically (newest) and a double-consume lands in the
-- idempotent upsert — accepted per rejected finding D1-m1.
--
-- FK ON DELETE CASCADE off context_api_keys (hypertable→plain FK is supported;
-- the reverse is not): deleting a key drains its stages — a deleted principal
-- must not be able to confirm anything.
--
-- lock_timeout (R-MIG2): CREATE TABLE / create_hypertable / CREATE INDEX on an
-- empty new table take only brief catalog locks. Forward-only, additive.
SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_pending_writes (
    id           UUID NOT NULL DEFAULT uuidv7(),
    api_key_id   UUID NOT NULL REFERENCES context_api_keys(id) ON DELETE CASCADE,
    scope        TEXT NOT NULL,                    -- write scope bound at stage time (fail-closed)
    op           TEXT NOT NULL,                    -- 'store' | 'update'
    origin       TEXT NOT NULL,                    -- 'mcp' | 'chat' (diagnosis)
    payload      JSONB NOT NULL,                   -- server-held, authoritative (tamper-proof: confirm carries only the hash)
    payload_hash TEXT NOT NULL,                    -- sha256(canonical(payload)) — canonicalization lands in D-W2
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ,                      -- NULL = never expires (confirm_ttl=0)
    consumed_at  TIMESTAMPTZ,                      -- NULL = open; set = consumed exactly once
    PRIMARY KEY (id, created_at)
);

-- if_not_exists=true lets the migration replay on a partially-applied DB (025).
SELECT create_hypertable(
    'context_pending_writes',
    'created_at',
    chunk_time_interval => interval '1 hour',
    if_not_exists => true
);

-- Consume/lookup selector: newest OPEN row per (key, hash). Partial index —
-- consumed history never widens the scan. (Partial non-unique indexes are fine
-- on hypertables; only UNIQUE needs the time column.)
CREATE INDEX IF NOT EXISTS idx_pending_open
    ON context_pending_writes (api_key_id, payload_hash, created_at DESC)
    WHERE consumed_at IS NULL;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (89, '089_pending_writes.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 089_pending_writes.sql

-- @@ ctx-fold begin 090_confirm_writes_capability.sql
-- =============================================================================
-- 090_confirm_writes_capability.sql — per-key confirm_writes capability (F6-C6 D-W4)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Adds context_api_keys.confirm_writes — a per-key OPT-IN flag that marks the
-- key's MCP store/update writes as stage-then-confirm (the D-W5 wave adds the
-- actual staged path; this migration is gate infrastructure only, no
-- behavioural change ships with it).
--
-- THREAT-MODEL NOTE (DECISIONS.md §Klarstellung D-E1/E2, 2026-07-05): gating
-- LLM-initiated writes is the HARNESS's responsibility (Claude Code /
-- claude.ai tool-approval; the ctx web chat's ConfirmCard). This flag is NOT a
-- security boundary — it is a per-principal distrust tool: the option to force
-- server-side staging onto a harness that has no (trusted) gating layer of its
-- own. Keys of harnesses with their own gating correctly keep it off.
--
-- DEFAULT false = fail-open for every existing key (decision D-E2): MCP
-- automations keep writing directly; nothing breaks on deploy. Opt-in is a
-- per-id UPDATE (same bootstrap convention as is_admin, 052).
--
-- ctx_auth is RETURNS TABLE, so the new return column forces DROP+CREATE (052/
-- 060/078 line; OR REPLACE refuses a return-type change). confirm_writes is
-- appended AS THE LAST column so every named-column SELECT (auth.go) and
-- AuthResult{} literal stays valid; an old binary that selects the original 9
-- columns keeps working (rollback compatibility). The body is byte-for-byte
-- the 078 body plus the one new column — no behavioural change to identity,
-- the tenant status gate, or the positional read_scopes build.
--
-- lock_timeout (R-MIG2): ADD COLUMN with a constant NOT NULL DEFAULT is a
-- catalog-only, non-rewriting change on PG11+; DROP/CREATE FUNCTION takes only
-- brief catalog locks. The runner wraps each migration in its own transaction,
-- so SET LOCAL is transaction-scoped and self-reverting.
-- Idempotent: ADD COLUMN IF NOT EXISTS + DROP FUNCTION IF EXISTS + CREATE
-- re-run cleanly; _migrations INSERT ON CONFLICT (version) DO NOTHING.
-- Forward-only. Function-only body change + one additive column → test.sh
-- table count UNCHANGED.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE context_api_keys
    ADD COLUMN IF NOT EXISTS confirm_writes BOOLEAN NOT NULL DEFAULT false;

DROP FUNCTION IF EXISTS ctx_auth(TEXT);

CREATE FUNCTION ctx_auth(p_api_key TEXT)
RETURNS TABLE (
    api_key_id     UUID,
    home_scope     VARCHAR(50),
    allowed_scopes TEXT[],
    read_scopes    TEXT[],
    is_valid       BOOLEAN,
    is_admin       BOOLEAN,
    tenant_id      UUID,
    tenant_role    TEXT,
    write_scopes   TEXT[],
    confirm_writes BOOLEAN   -- NEW (F6-C6 D-W4): per-key stage-then-confirm
                             -- opt-in; RAW column value, evaluated at the
                             -- store handlers only (D-W5)
) LANGUAGE plpgsql AS $$
DECLARE
    v_key_hash       TEXT;
    v_api_key_id     UUID;
    v_home_scope     VARCHAR(50);
    v_allowed_scopes TEXT[];
    v_is_admin       BOOLEAN;
    v_tenant_id      UUID;
    v_tenant_role    TEXT;
    v_status         TEXT;
    v_read_scopes    TEXT[];
    v_cand           TEXT[];
    v_s              TEXT;
    v_write_scopes   TEXT[];
    v_confirm_writes BOOLEAN;     -- NEW
BEGIN
    v_key_hash := encode(digest(p_api_key, 'sha256'), 'hex');

    UPDATE context_api_keys
    SET last_used_at = now()
    WHERE context_api_keys.key_hash = v_key_hash
      AND context_api_keys.active = true
    RETURNING
        context_api_keys.id,
        context_api_keys.home_scope,
        context_api_keys.allowed_scopes,
        context_api_keys.is_admin,
        context_api_keys.tenant_id,
        context_api_keys.tenant_role,
        context_api_keys.write_scopes,
        context_api_keys.confirm_writes
    INTO v_api_key_id, v_home_scope, v_allowed_scopes, v_is_admin, v_tenant_id, v_tenant_role, v_write_scopes, v_confirm_writes;

    -- Key miss: sentinel (unchanged shape; +1 explicit new column).
    IF v_api_key_id IS NULL THEN
        api_key_id     := NULL;
        home_scope     := '__UNAUTHORIZED__';
        allowed_scopes := '{}'::TEXT[];
        read_scopes    := '{}'::TEXT[];
        is_valid       := false;
        is_admin       := false;
        tenant_id      := NULL;
        tenant_role    := '';
        write_scopes   := '{}'::TEXT[];
        confirm_writes := false;
        RETURN NEXT;
        RETURN;
    END IF;

    -- Tenant status gate (design/01 §5.2), BEFORE the read_scopes build.
    SELECT status INTO v_status FROM context_tenants WHERE id = v_tenant_id;
    IF v_status IS NULL OR v_status <> 'active' THEN
        api_key_id     := NULL;
        home_scope     := '__UNAUTHORIZED__';
        allowed_scopes := '{}'::TEXT[];
        read_scopes    := '{}'::TEXT[];
        is_valid       := false;
        is_admin       := false;
        tenant_id      := NULL;
        tenant_role    := '';
        write_scopes   := '{}'::TEXT[];
        confirm_writes := false;
        RETURN NEXT;
        RETURN;
    END IF;

    -- read_scopes POSITIONAL (design/02 §4.1 amendment, Variante A). Unchanged.
    v_read_scopes := ARRAY[v_home_scope::TEXT];
    v_cand := COALESCE(v_allowed_scopes, '{}'::TEXT[])
           || COALESCE((SELECT array_agg(g.granted_scope ORDER BY g.granted_scope)
                          FROM context_tenant_grants g
                         WHERE g.grantee_tenant = v_tenant_id), '{}'::TEXT[]);
    FOREACH v_s IN ARRAY v_cand LOOP
        IF v_s NOT LIKE '\_%' AND NOT (v_s = ANY(v_read_scopes)) THEN
            v_read_scopes := v_read_scopes || v_s;
        END IF;
    END LOOP;

    -- Valid key (+1 new column). confirm_writes is returned RAW (COALESCE
    -- floor to false for a NULL column); it is consumed at the store handlers
    -- only (HandleStore / mcpStoreHandler, D-W5) — internal writers (digest,
    -- dream) go through store.UpsertBlock and never see it.
    api_key_id     := v_api_key_id;
    home_scope     := v_home_scope;
    allowed_scopes := v_allowed_scopes;
    read_scopes    := COALESCE(NULLIF(v_read_scopes, '{}'::TEXT[]), ARRAY[v_home_scope]::TEXT[]);
    is_valid       := true;
    is_admin       := v_is_admin;
    tenant_id      := v_tenant_id;
    tenant_role    := v_tenant_role;
    write_scopes   := COALESCE(v_write_scopes, '{}'::TEXT[]);
    confirm_writes := COALESCE(v_confirm_writes, false);
    RETURN NEXT;
    RETURN;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (90, '090_confirm_writes_capability.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 090_confirm_writes_capability.sql

-- @@ ctx-fold begin 091_dispatch_telemetry.sql
-- =============================================================================
-- 091_dispatch_telemetry.sql — Dispatch-Telemetrie im LLM-Log (Vorhaben E, MW10/A5-W4)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Persistiert die Lease-Telemetrie des Admission-Layers (internal/dispatch)
-- pro llmlog-Zeile (design/05 §3.1/§3.2):
--   queue_wait_ms  — Lease-Wartezeit des zeilen-prägenden Attempts (admitted −
--                    enqueued). 0 ist ein ECHTER Messwert (Sofort-Admission,
--                    auch im Durchreiche-Zustand) und wird als 0 persistiert,
--                    nie zu NULL gedroppt (B-R4 — sonst wäre jede p95-
--                    Auswertung nach oben verzerrt). NULL = Zeile aus
--                    Vor-Verdrahtungs-Zeit oder lease-freier Sonderpfad.
--   dispatch_class — 'interactive' | 'background' (vom Caller gebundene
--                    Admissions-Klasse der Sequenz). NULL = Vor-Verdrahtung.
--   dispatch_abort — 'preempted' | 'reaped' (Dispatcher-Abbruch eines
--                    laufenden Attempts) | 'acquire_expired' | 'queue_full'
--                    (K9-Abweis-Zeile: nie-admittierter background-Acquire,
--                    duration_ms NULL — kein physischer Call). NULL = kein
--                    Dispatcher-Eingriff (auch bei gewöhnlichen Fehlern).
--                    Klassen-Invariante: nur background-Zeilen tragen je einen
--                    Wert (I-D1: interactive wird nie dispatcher-gecancelt).
-- KEIN CHECK-Constraint auf die Wertemengen (Bestandsmuster backend_trust/
-- required_sensitivity: freie text-Spalten); die Vokabulare pinnt ein Go-Test
-- (llmlog/chain-Testgates, A5-W4).
--
-- Hypertable-Kosten (B-R6): alle drei Spalten nullable OHNE Default ⇒ reine
-- Katalog-Operation ohne Chunk-Rewrite auf der TimescaleDB-Hypertable;
-- Bestands-Zeilen bleiben byte-gleich lesbar (Integrations-Probe auf chunk-
-- bestückter Test-Hypertable). Der Partial-Index folgt exakt dem Muster
-- idx_llm_log_error (M025): Abbruch-/Abweis-Ereignisse sind selten,
-- "Preemptions/Starvation-Abweise pro Tag/Ziel" müssen am 1M+-Ziel-Scale
-- trotzdem ohne Chunk-Vollscan zählbar sein.
--
-- lock_timeout (R-MIG2): ADD COLUMN nullable ohne Default ist katalog-only;
-- der Runner wickelt jede Migration in eine eigene Transaktion, SET LOCAL ist
-- transaktions-gebunden und selbst-revertierend.
-- Idempotent: IF NOT EXISTS überall; _migrations ON CONFLICT DO NOTHING.
-- Forward-only. Additive Spalten, keine neue Tabelle → test.sh table count
-- UNCHANGED. EvictBodies NULLt die neuen Spalten bewusst NICHT (Telemetrie
-- überlebt die Body-Retention, Bestands-Doktrin llmlog.go).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE context_llm_log
    ADD COLUMN IF NOT EXISTS queue_wait_ms  INTEGER,
    ADD COLUMN IF NOT EXISTS dispatch_class TEXT,
    ADD COLUMN IF NOT EXISTS dispatch_abort TEXT;

CREATE INDEX IF NOT EXISTS idx_llm_log_dispatch_abort
    ON context_llm_log (created_at DESC) WHERE dispatch_abort IS NOT NULL;

INSERT INTO _migrations (version, filename, applied_at)
VALUES (91, '091_dispatch_telemetry.sql', now()) ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 091_dispatch_telemetry.sql

-- @@ ctx-fold begin 092_disable_profiles.sql
-- =============================================================================
-- 092_disable_profiles.sql — Abschaltprofile (Design plan-webux 01, User-Auftrag
-- 2026-07-06; Wellen-Achse U01-W1)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Benannte Profile nehmen je eine Menge Backends aus jeder Chain (Wartungs-/
-- Eject-Abschaltung). Der bisher hartkodierte Gaming-Sonderfall (gaming.active +
-- Namensliste gaming.disabled_backends, config.go:531/537) wird zu EINEM Profil
-- unter mehreren; der Backfill unten kopiert Wert + Menge verlustfrei.
--
-- AM-5 (U01-E4=scoped): context_disable_profiles trägt scope ab W1; W1 legt nur
-- das Schema, der Sichtbarkeits-/Rechte-Filter kommt in W3. Das Backfill-Profil
-- ist scope='_global'. UNIQUE ist (scope, name).
-- AM-7 (Rename gaming→eject): das Backfill-Profil heißt 'eject' / 'Eject-Modus';
-- der gaming-mode-Shim mappt in W3 auf dieses Profil (gaming = Alias).
--
-- Doktrin: Profile gaten physische Hosts (dieselbe Linie wie gaming.active
-- "gates a physical GPU host"). Tenant-Selbst-Abschaltung bleibt das
-- enabled-Flag (053:41), kein Profil.
-- =============================================================================

CREATE TABLE IF NOT EXISTS context_disable_profiles (
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    scope       VARCHAR(50) NOT NULL DEFAULT '_global',      -- AM-5: Sichtbarkeit ab W3;
                                                             -- W1-Backfill = '_global'
    name        TEXT NOT NULL
                CHECK (name ~ '^[a-z0-9][a-z0-9_-]{0,62}$'),  -- slug: URL-/CLI-tauglich
    label       TEXT NOT NULL DEFAULT ''                      -- Anzeige, frei
                CHECK (char_length(label) <= 120),            -- Layout-/title-Schranke (§5.3)
    description TEXT NOT NULL DEFAULT ''                       -- Erstnutzer-Hint (§4.6)
                CHECK (char_length(description) <= 500),       -- fremd-tenant-sichtbar
    active      BOOLEAN NOT NULL DEFAULT false,               -- fail-closed: neu = wirkungslos
    reserved    BOOLEAN NOT NULL DEFAULT false,               -- Break-Glass-Schutz: eject-Profil
                                                              -- ist im Cutover-Fenster nicht löschbar (§4.3)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_disable_profiles_scope_name UNIQUE (scope, name)  -- AM-5: (scope,name) statt name
);

CREATE TABLE IF NOT EXISTS context_disable_profile_backends (
    profile_id UUID NOT NULL REFERENCES context_disable_profiles(id) ON DELETE CASCADE,
    backend_id UUID NOT NULL REFERENCES context_backends(id)         ON DELETE CASCADE,
    PRIMARY KEY (profile_id, backend_id)
);

-- Hot-Reload: gleicher Kanal/gleiche Funktion wie 051/053/065 — notify_settings_write()
-- liest entity=TG_TABLE_NAME und identität via to_jsonb(COALESCE(NEW,OLD))->>'key'/'name'.
-- Der Go-Listener (events/listener.go, N9) dispatcht beide Entities in den
-- Pool-Reload-Arm (§4.1). Die früher erwogene COALESCE(key,name,'')-Anpassung
-- entfällt: ein NULL-key ist im Payload bereits inert (Listener liest entity/scope,
-- nie key), und die Funktion ist von 5+ Entities geteilt.
DROP TRIGGER IF EXISTS trg_disable_profiles_notify ON context_disable_profiles;
CREATE TRIGGER trg_disable_profiles_notify
    AFTER INSERT OR UPDATE OR DELETE ON context_disable_profiles
    FOR EACH ROW EXECUTE FUNCTION notify_settings_write();

-- Join-Trigger: der Payload-`key` ist hier BEWUSST NULL — die Tabelle trägt weder
-- eine key- noch eine name-Spalte, to_jsonb(...)->>'key'/'name' liefert SQL NULL
-- (kein Fehler). Der Listener routet allein über entity=TG_TABLE_NAME.
DROP TRIGGER IF EXISTS trg_disable_profile_backends_notify ON context_disable_profile_backends;
CREATE TRIGGER trg_disable_profile_backends_notify
    AFTER INSERT OR UPDATE OR DELETE ON context_disable_profile_backends
    FOR EACH ROW EXECUTE FUNCTION notify_settings_write();

-- Audit im Trigger (Muster audit_backends_write, 053:71-116) OHNE Redaction:
-- Profile/Memberships tragen keine Secrets. Append-only in dieselbe
-- context_settings_audit-Tabelle; deckt API, psql und break-glass gleichermaßen.
CREATE OR REPLACE FUNCTION audit_disable_profiles_write() RETURNS TRIGGER AS $$
DECLARE
    v_new   JSONB := to_jsonb(NEW);
    v_old   JSONB := to_jsonb(OLD);
    v_actor UUID  := NULLIF(current_setting('ctx.api_key_id', true), '')::uuid;
    v_label TEXT;
BEGIN
    IF v_actor IS NOT NULL THEN
        SELECT label INTO v_label FROM context_api_keys WHERE id = v_actor;
    END IF;
    INSERT INTO context_settings_audit
        (entity_type, entity_key, scope, action, old_value, new_value,
         api_key_id, actor_label, metadata)
    VALUES (
        'disable_profile',
        COALESCE(v_new->>'name', v_old->>'name'),
        COALESCE(v_new->>'scope', v_old->>'scope', '_global'),
        CASE TG_OP WHEN 'INSERT' THEN 'create'
                   WHEN 'UPDATE' THEN 'update'
                   ELSE 'delete' END,
        v_old, v_new,
        v_actor, v_label,
        jsonb_strip_nulls(jsonb_build_object(
            'via',        CASE WHEN v_actor IS NULL THEN 'sql' ELSE 'api' END,
            'request_id', NULLIF(current_setting('ctx.request_id', true), '')))
    );
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_disable_profiles_audit ON context_disable_profiles;
CREATE TRIGGER trg_disable_profiles_audit
    AFTER INSERT OR UPDATE OR DELETE ON context_disable_profiles
    FOR EACH ROW EXECUTE FUNCTION audit_disable_profiles_write();

CREATE OR REPLACE FUNCTION audit_disable_profile_backends_write() RETURNS TRIGGER AS $$
DECLARE
    v_new   JSONB := to_jsonb(NEW);
    v_old   JSONB := to_jsonb(OLD);
    v_actor UUID  := NULLIF(current_setting('ctx.api_key_id', true), '')::uuid;
    v_label TEXT;
BEGIN
    IF v_actor IS NOT NULL THEN
        SELECT label INTO v_label FROM context_api_keys WHERE id = v_actor;
    END IF;
    -- scope: Memberships gaten physische _global-Hosts (§3.2-Doktrin); der Join
    -- trägt selbst kein scope, das Audit schreibt daher '_global'. entity_key =
    -- profile_id (das getroffene Profil).
    INSERT INTO context_settings_audit
        (entity_type, entity_key, scope, action, old_value, new_value,
         api_key_id, actor_label, metadata)
    VALUES (
        'disable_profile_backend',
        COALESCE(v_new->>'profile_id', v_old->>'profile_id'),
        '_global',
        CASE TG_OP WHEN 'INSERT' THEN 'create'
                   WHEN 'UPDATE' THEN 'update'
                   ELSE 'delete' END,
        v_old, v_new,
        v_actor, v_label,
        jsonb_strip_nulls(jsonb_build_object(
            'via',        CASE WHEN v_actor IS NULL THEN 'sql' ELSE 'api' END,
            'request_id', NULLIF(current_setting('ctx.request_id', true), '')))
    );
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_disable_profile_backends_audit ON context_disable_profile_backends;
CREATE TRIGGER trg_disable_profile_backends_audit
    AFTER INSERT OR UPDATE OR DELETE ON context_disable_profile_backends
    FOR EACH ROW EXECUTE FUNCTION audit_disable_profile_backends_write();

-- ---------------------------------------------------------------------------
-- Backfill (Bestandsdaten-Pfad, idempotent): der letzte Auftritt des
-- Namens-Match-Typo-Problems — danach strukturell unmöglich (FK + CASCADE).
-- Idempotent per WHERE NOT EXISTS name='eject' (RETURN beim Zweitlauf).
-- Kein Settings-Delete: gaming.active bleibt liegen bis U01-W5 den Reader zieht.
-- ---------------------------------------------------------------------------
DO $$
DECLARE
    v_profile_id UUID;
    v_active     BOOLEAN;
    v_names      TEXT[];
    v_name       TEXT;
    v_backend_id UUID;
    v_matched    INT := 0;
BEGIN
    IF EXISTS (SELECT 1 FROM context_disable_profiles WHERE scope = '_global' AND name = 'eject') THEN
        RAISE NOTICE '092 Backfill: Profil eject existiert bereits — übersprungen.';
        RETURN;
    END IF;

    -- active aus der Settings-Row gaming.active (scope '_global'), sonst false.
    SELECT (value #>> '{}')::boolean INTO v_active
      FROM context_settings
     WHERE key = 'gaming.active' AND scope = '_global';
    IF v_active IS NULL THEN
        v_active := false;
    END IF;

    -- Member-Quelle: Settings-Row gaming.disabled_backends (comma-split), sonst
    -- der Code-Default aus config.go:537 (herbert-chat,herbert-rerank) als Literal.
    SELECT string_to_array(value #>> '{}', ',') INTO v_names
      FROM context_settings
     WHERE key = 'gaming.disabled_backends' AND scope = '_global';
    IF v_names IS NULL THEN
        v_names := ARRAY['herbert-chat', 'herbert-rerank'];
    END IF;

    INSERT INTO context_disable_profiles (scope, name, label, description, active, reserved)
    VALUES ('_global', 'eject', 'Eject-Modus',
            'Nimmt die GPU-Backends aus jeder Chain — Failover (CPU/extern) übernimmt. Laufende Requests beenden normal.',
            v_active, true)
    RETURNING id INTO v_profile_id;

    FOREACH v_name IN ARRAY v_names LOOP
        v_name := btrim(v_name);
        CONTINUE WHEN v_name = '';
        SELECT id INTO v_backend_id
          FROM context_backends
         WHERE scope = '_global' AND name = v_name;
        IF v_backend_id IS NULL THEN
            RAISE NOTICE '092 Backfill: Backend % (scope _global) nicht gefunden — Membership übersprungen.', v_name;
            CONTINUE;
        END IF;
        INSERT INTO context_disable_profile_backends (profile_id, backend_id)
        VALUES (v_profile_id, v_backend_id)
        ON CONFLICT DO NOTHING;
        v_matched := v_matched + 1;
    END LOOP;

    RAISE NOTICE '092 Backfill: Profil eject angelegt (active=%), % Member verknüpft.', v_active, v_matched;
END $$;

INSERT INTO _migrations (version, filename, applied_at)
VALUES (92, '092_disable_profiles.sql', now()) ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 092_disable_profiles.sql

-- @@ ctx-fold begin 093_graph_category_hues.sql
-- =============================================================================
-- 093_graph_category_hues.sql — per-category HUE override for the graph (AM-2,
-- Design plan-webux 02a, Wellen-Achse U02-W5)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- AM-2: the node/cluster colour of a block CATEGORY is a seeded hash by default
-- (categoryColor → hashHue, graph-client.ts); this table is the OPTIONAL override
-- layer. Resolution chain (02a §A3): tenant-override → global-override → hash
-- seed. Only the HUE (HSL degree 0–359) is overridden — sat/lum stay theme
-- tokens, so every override lands inside the range the G1a contrast sweep already
-- covers (02a §A2: no override-specific contrast gate needed).
--
-- Scope discriminator like 092:34 (VARCHAR(50), Modell C — NO tenant_id on data
-- tables). UNIQUE(scope, category): one row per (scope, category), NOT a JSON
-- map — this kills the read-modify-write patch race (02a §A5) and gives per-key
-- precedence structurally (two concurrent overrides of different categories hit
-- different rows via ON CONFLICT).
--
-- No slug-CHECK on category: context_blocks.category is DB-side free (a Bestands-
-- Kategorie must not be excluded from an override); render-safety comes from the
-- FE-pin (02a §A1: Map structure + Svelte text-interpolation, {@html} banned).
-- The CHECK is length + no control chars only.
--
-- BEWUSST KEIN notify_settings_write-Trigger (02a §A1, Review-Korrektur, 2 Linsen
-- konvergent): there is NO server cache — the GET reads live (02a §A3/§A4-W5) —
-- and the Ist-Listener (events/listener.go:175-188) routes UNKNOWN entities into
-- the config-reload fall-through, so a _global hue edit would fire a full
-- settings.Reload + Dispatch-UpdateSettings across every tenant per PUT, zero
-- benefit at real churn. Should a server cache ever land, it needs its OWN
-- listener branch (entity='context_graph_category_hues'), never the fall-through.
-- =============================================================================

CREATE TABLE IF NOT EXISTS context_graph_category_hues (
    scope      VARCHAR(50) NOT NULL DEFAULT '_global',   -- discriminator like 092:34
    category   TEXT NOT NULL CHECK (char_length(category) BETWEEN 1 AND 128
                                    AND category !~ '[[:cntrl:]]'),
    hue        SMALLINT NOT NULL CHECK (hue >= 0 AND hue <= 359),  -- HSL degree (02a §A2)
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by UUID,
    CONSTRAINT uq_graph_cat_hue_scope_cat UNIQUE (scope, category)
);
-- The UNIQUE(scope, category) index already backs the precedence read
-- (WHERE scope = ANY(...)) — no additional index (02a §A1).

-- Audit WITHOUT redaction (a hue carries no secret) into the shared
-- context_settings_audit — function skeleton 1:1 from audit_disable_profiles_write
-- (092, EXECUTE-Muster 092:55,63): entity_type='graph_category_hue',
-- entity_key=COALESCE(NEW,OLD)->>'category', scope from the row, action per TG_OP,
-- via='api'|'sql' from ctx.api_key_id (NULL ⇒ 'sql' — covers psql/break-glass,
-- 092:72,91,129).
CREATE OR REPLACE FUNCTION audit_graph_category_hues_write() RETURNS TRIGGER AS $$
DECLARE
    v_new   JSONB := to_jsonb(NEW);
    v_old   JSONB := to_jsonb(OLD);
    v_actor UUID  := NULLIF(current_setting('ctx.api_key_id', true), '')::uuid;
    v_label TEXT;
BEGIN
    IF v_actor IS NOT NULL THEN
        SELECT label INTO v_label FROM context_api_keys WHERE id = v_actor;
    END IF;
    INSERT INTO context_settings_audit
        (entity_type, entity_key, scope, action, old_value, new_value,
         api_key_id, actor_label, metadata)
    VALUES (
        'graph_category_hue',
        COALESCE(v_new->>'category', v_old->>'category'),
        COALESCE(v_new->>'scope', v_old->>'scope', '_global'),
        CASE TG_OP WHEN 'INSERT' THEN 'create'
                   WHEN 'UPDATE' THEN 'update'
                   ELSE 'delete' END,
        v_old, v_new,
        v_actor, v_label,
        jsonb_strip_nulls(jsonb_build_object(
            'via',        CASE WHEN v_actor IS NULL THEN 'sql' ELSE 'api' END,
            'request_id', NULLIF(current_setting('ctx.request_id', true), '')))
    );
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_graph_category_hues_audit ON context_graph_category_hues;
CREATE TRIGGER trg_graph_category_hues_audit
    AFTER INSERT OR UPDATE OR DELETE ON context_graph_category_hues
    FOR EACH ROW EXECUTE FUNCTION audit_graph_category_hues_write();

-- No backfill: overrides are the exception layer — an empty table means every
-- category renders on its hash seed (the correct start behaviour, 02a §A1).

INSERT INTO _migrations (version, filename, applied_at)
VALUES (93, '093_graph_category_hues.sql', now()) ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 093_graph_category_hues.sql

-- @@ ctx-fold begin 094_principal_identity.sql
-- =============================================================================
-- 094_principal_identity.sql — Principal-/Identity-Fundament (OAuth-Achse 01-P1)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Fundament-Welle F1 des OAuth-Stack-Ausbaus (Plan .project/plan-oauth-2026-07-09/,
-- Design 01 §4/§8-P1, Entscheidung E1=B' Big-Bang mit additivem Audit).
--
-- Führt eine echte Principal-Entität ein, auf der die drei Auth-Wege
-- (api_key-Bearer, OAuth-Token, externes Login) konvergieren. DIESE Welle legt
-- NUR das Datenmodell an — ctx_auth() bleibt unangetastet (das ist F2/Mig 095),
-- also ist die Bestands-Authentifizierung nach dieser Migration byte-identisch.
--
--   context_principals          — die Person/Identität (id, display_name, is_active)
--   context_external_identities — externe IdP-Verknüpfung (Achse 04 füllt/verfeinert)
--   context_api_keys.principal_id NOT NULL — jeder Key gehört genau einem Principal
--
-- BACKFILL (grüne Wiese, ~10 Keys): jeder bestehende api_key bekommt einen
-- eigenen frisch angelegten Principal. principal_id wird erst nach dem Backfill
-- auf NOT NULL gesetzt, damit die Migration auf einer befüllten Tabelle läuft.
--
-- HA-SAFE (Design 01 §7): Der Runner (migrations.go) nimmt KEINEN globalen Lock,
-- bevor er den Migrations-Body ausführt — zwei parallel startende ctxd-Instanzen
-- könnten beide die EXISTS-Prüfung passieren und den Backfill doppelt ausführen
-- → doppelte Principals. pg_advisory_xact_lock serialisiert den Body; die zweite
-- Instanz blockt bis zum Commit der ersten und findet dann WHERE principal_id IS
-- NULL leer vor. Der Lock ist transaktions-gebunden (Runner wrappt jede Migration
-- in eine eigene tx) und löst sich beim Commit selbst.
--
-- Alle Schritte idempotent (CREATE TABLE IF NOT EXISTS, ADD COLUMN IF NOT EXISTS,
-- Backfill WHERE principal_id IS NULL, SET NOT NULL ist auf bereits-NOT-NULL ein
-- No-Op, FK-Constraint per duplicate_object-Guard). Forward-only.
--
-- lock_timeout (R-MIG2): ADD COLUMN nullable + CREATE TABLE sind katalog-leichte
-- Operationen; SET NOT NULL scannt die (winzige) Tabelle einmal. SET LOCAL ist
-- transaktions-scoped und selbst-revertierend.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- Serialisiert konkurrierende Migrations-Läufe (HA, Design 01 §7).
SELECT pg_advisory_xact_lock(94094094);

-- --- Principal-Entität --------------------------------------------------------
CREATE TABLE IF NOT EXISTS context_principals (
    id            UUID PRIMARY KEY DEFAULT uuidv7(),
    display_name  VARCHAR,
    primary_email VARCHAR,                         -- aus externem Login, NICHT auth-relevant
    is_active     BOOLEAN NOT NULL DEFAULT true,   -- in ctx_auth enforced (F2/095), kein Deko-Feld
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- --- Externe Identitäten (Achse 04 füllt) ------------------------------------
CREATE TABLE IF NOT EXISTS context_external_identities (
    id           UUID PRIMARY KEY DEFAULT uuidv7(),
    principal_id UUID NOT NULL REFERENCES context_principals(id) ON DELETE CASCADE,
    provider     VARCHAR(50) NOT NULL,   -- 'github' | 'oidc'
    issuer       VARCHAR NOT NULL,       -- allowlisted, validierter Issuer (INV-C), NICHT rohe iss-Claim
    subject      VARCHAR NOT NULL,
    email        VARCHAR,
    verified_at  TIMESTAMPTZ,            -- subject-Reuse-Schutz
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (issuer, subject)
);

-- --- api_keys.principal_id (nullable, dann Backfill, dann NOT NULL) -----------
ALTER TABLE context_api_keys ADD COLUMN IF NOT EXISTS principal_id UUID;

-- Backfill: ein Principal pro noch nicht verknüpftem Key (grüne Wiese, 1:1).
-- Per-Row-Schleife, weil ein Set-INSERT die 1:1-Rückverknüpfung Key↔Principal
-- nicht sauber ausdrücken kann (label ist nicht eindeutig).
DO $$
DECLARE
    k        RECORD;
    v_pid    UUID;
    v_count  INTEGER := 0;
BEGIN
    FOR k IN SELECT id, label FROM context_api_keys WHERE principal_id IS NULL LOOP
        INSERT INTO context_principals (display_name) VALUES (k.label)
            RETURNING id INTO v_pid;
        UPDATE context_api_keys SET principal_id = v_pid WHERE id = k.id;
        v_count := v_count + 1;
    END LOOP;
    RAISE NOTICE '094 Backfill: % Key(s) mit frischem Principal verknüpft.', v_count;
END $$;

-- Jetzt ist die Spalte befüllt → NOT NULL + FK (ON DELETE RESTRICT: keine
-- rechte-erhaltenden Waisen, Principal-Hard-Delete erzwingt vorherige
-- Key-Deaktivierung, Design 01 §4).
ALTER TABLE context_api_keys ALTER COLUMN principal_id SET NOT NULL;

DO $$ BEGIN
    ALTER TABLE context_api_keys
        ADD CONSTRAINT context_api_keys_principal_id_fkey
        FOREIGN KEY (principal_id) REFERENCES context_principals(id) ON DELETE RESTRICT;
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

INSERT INTO _migrations (version, filename, applied_at)
VALUES (94, '094_principal_identity.sql', now()) ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 094_principal_identity.sql

-- @@ ctx-fold begin 095_ctx_auth_principal.sql
-- =============================================================================
-- 095_ctx_auth_principal.sql — ctx_auth + ctx_auth_by_id Principal-Konvergenz
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Fundament-Welle F2 des OAuth-Stack-Ausbaus (Design 01 §8-P2, K3 Masterplan).
-- Baut auf F1/Mig 094 (context_principals + api_keys.principal_id NOT NULL) auf.
--
-- ZWEI Änderungen an der Auth-Funktion:
--
-- (1) ctx_auth() liefert principal_id als NEUE, LETZTE Spalte (Append-Pattern
--     wie 078/090 — jeder benannte SELECT bleibt gültig, alte Binaries lesen
--     die ersten 10 Spalten weiter) und enforced ein DRITTES fail-closed-Gate:
--     der Principal muss is_active sein. Das ist der Revoke-all-Mechanismus
--     (Design 01 §4): principal.is_active=false sperrt ALLE Keys der Person
--     ohne Key-für-Key-Deaktivierung.
--
-- (2) Der innere Scope-Build wird in ctx_auth_by_id(uuid) AUSFAKTORISIERT (K3):
--     ctx_auth(key_plaintext) löst nur key_hash→api_key_id auf und delegiert per
--     RETURN QUERY an ctx_auth_by_id. Achse 03 (Token-Auflösung am /mcp) und
--     Achse 05 (Browser-Session) konsumieren DIESELBE Funktion — kein Zwilling,
--     kein Gate-Drift. ctx_auth_by_id spiegelt ALLE drei fail-closed-Gates
--     (Key active, Tenant status, Principal is_active) UND den last_used_at-Write.
--     Ein neues Auth-Gate, das je nur in einen der Pfade wanderte, ist damit
--     strukturell ausgeschlossen (Risiko #1 Masterplan).
--
-- BYTE-IDENTITÄT (F2-Gate): für einen gültigen, aktiven Key mit aktivem
-- Principal ist die zurückgegebene Autorisierung (scopes, tenant, admin, …)
-- unverändert gegenüber 090 — nur principal_id kommt hinzu. Der Split
-- verändert das Verhalten nicht: ctx_auth macht statt eines UPDATE-by-hash nun
-- ein SELECT-by-hash + delegierten UPDATE-by-id; ein inaktiver/fehlender Key
-- führt in beiden Fassungen zur Sentinel-Antwort, last_used_at wird nur bei
-- active=true berührt.
--
-- SOFT-REVOKE-NEGATIV-GATE (Design 01 §8-P2, eigenes Gate): ein Key mit
-- active=false resolvt über ctx_auth_by_id zu KEINEM AuthResult — der api_key-
-- Parity-Test fängt das nicht, weil der by-id-Zweig die active-Klausel umgehen
-- könnte; hier ist sie im UPDATE-WHERE fest verankert.
--
-- lock_timeout (R-MIG2): reine Funktions-DDL (DROP/CREATE), nur kurze
-- Katalog-Locks. SET LOCAL ist transaktions-scoped + selbst-revertierend.
-- Idempotent: DROP FUNCTION IF EXISTS + CREATE re-runnen sauber; keine
-- Tabellen-Änderung → test.sh table count UNVERÄNDERT. Forward-only (Revert =
-- vorherigen 090-Body re-applyen).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- ctx_auth ändert die RETURN-Signatur (neue Spalte) → DROP erzwungen
-- (OR REPLACE verweigert eine Return-Type-Änderung). ctx_auth_by_id neu.
DROP FUNCTION IF EXISTS ctx_auth(TEXT);
DROP FUNCTION IF EXISTS ctx_auth_by_id(UUID);

-- --- ctx_auth_by_id: der geteilte innere Scope-Build (K3) --------------------
-- Eingang: eine api_key_id (aus key_hash-Auflösung, aus einem OAuth-Token oder
-- aus einer Browser-Session — INV-A: immer GENAU EIN Key). Führt die drei
-- fail-closed-Gates + last_used_at + read_scopes-Build und liefert die volle
-- AuthResult-Zeile inkl. principal_id.
CREATE FUNCTION ctx_auth_by_id(p_api_key_id UUID)
RETURNS TABLE (
    api_key_id     UUID,
    home_scope     VARCHAR(50),
    allowed_scopes TEXT[],
    read_scopes    TEXT[],
    is_valid       BOOLEAN,
    is_admin       BOOLEAN,
    tenant_id      UUID,
    tenant_role    TEXT,
    write_scopes   TEXT[],
    confirm_writes BOOLEAN,
    principal_id   UUID       -- NEW (F2): Person-Attribution; NOT NULL auf api_keys (094)
) LANGUAGE plpgsql AS $$
DECLARE
    v_api_key_id       UUID;
    v_home_scope       VARCHAR(50);
    v_allowed_scopes   TEXT[];
    v_is_admin         BOOLEAN;
    v_tenant_id        UUID;
    v_tenant_role      TEXT;
    v_status           TEXT;
    v_read_scopes      TEXT[];
    v_cand             TEXT[];
    v_s                TEXT;
    v_write_scopes     TEXT[];
    v_confirm_writes   BOOLEAN;
    v_principal_id     UUID;
    v_principal_active BOOLEAN;
BEGIN
    -- Gate 1: Key existiert UND ist active (soft-revoke-Gate). UPDATE-by-id
    -- berührt last_used_at nur bei active=true — der Soft-Revoke-Negativ-Gate.
    UPDATE context_api_keys
    SET last_used_at = now()
    WHERE context_api_keys.id = p_api_key_id
      AND context_api_keys.active = true
    RETURNING
        context_api_keys.id,
        context_api_keys.home_scope,
        context_api_keys.allowed_scopes,
        context_api_keys.is_admin,
        context_api_keys.tenant_id,
        context_api_keys.tenant_role,
        context_api_keys.write_scopes,
        context_api_keys.confirm_writes,
        context_api_keys.principal_id
    INTO v_api_key_id, v_home_scope, v_allowed_scopes, v_is_admin, v_tenant_id,
         v_tenant_role, v_write_scopes, v_confirm_writes, v_principal_id;

    IF v_api_key_id IS NULL THEN
        api_key_id := NULL; home_scope := '__UNAUTHORIZED__'; allowed_scopes := '{}'::TEXT[];
        read_scopes := '{}'::TEXT[]; is_valid := false; is_admin := false;
        tenant_id := NULL; tenant_role := ''; write_scopes := '{}'::TEXT[];
        confirm_writes := false; principal_id := NULL;
        RETURN NEXT; RETURN;
    END IF;

    -- Gate 2: Tenant status (design/01 §5.2), VOR dem read_scopes-Build.
    SELECT status INTO v_status FROM context_tenants WHERE id = v_tenant_id;
    IF v_status IS NULL OR v_status <> 'active' THEN
        api_key_id := NULL; home_scope := '__UNAUTHORIZED__'; allowed_scopes := '{}'::TEXT[];
        read_scopes := '{}'::TEXT[]; is_valid := false; is_admin := false;
        tenant_id := NULL; tenant_role := ''; write_scopes := '{}'::TEXT[];
        confirm_writes := false; principal_id := NULL;
        RETURN NEXT; RETURN;
    END IF;

    -- Gate 3 (NEW, F2): Principal is_active — der Revoke-all-Mechanismus.
    -- principal_id ist NOT NULL auf api_keys (094), aber der Principal selbst
    -- kann deaktiviert sein → sperrt ALLE seine Keys, key-übergreifend.
    SELECT is_active INTO v_principal_active FROM context_principals WHERE id = v_principal_id;
    IF v_principal_active IS NULL OR v_principal_active = false THEN
        api_key_id := NULL; home_scope := '__UNAUTHORIZED__'; allowed_scopes := '{}'::TEXT[];
        read_scopes := '{}'::TEXT[]; is_valid := false; is_admin := false;
        tenant_id := NULL; tenant_role := ''; write_scopes := '{}'::TEXT[];
        confirm_writes := false; principal_id := NULL;
        RETURN NEXT; RETURN;
    END IF;

    -- read_scopes POSITIONAL (design/02 §4.1 Variante A) — unverändert ggü. 090.
    v_read_scopes := ARRAY[v_home_scope::TEXT];
    v_cand := COALESCE(v_allowed_scopes, '{}'::TEXT[])
           || COALESCE((SELECT array_agg(g.granted_scope ORDER BY g.granted_scope)
                          FROM context_tenant_grants g
                         WHERE g.grantee_tenant = v_tenant_id), '{}'::TEXT[]);
    FOREACH v_s IN ARRAY v_cand LOOP
        IF v_s NOT LIKE '\_%' AND NOT (v_s = ANY(v_read_scopes)) THEN
            v_read_scopes := v_read_scopes || v_s;
        END IF;
    END LOOP;

    api_key_id     := v_api_key_id;
    home_scope     := v_home_scope;
    allowed_scopes := v_allowed_scopes;
    read_scopes    := COALESCE(NULLIF(v_read_scopes, '{}'::TEXT[]), ARRAY[v_home_scope]::TEXT[]);
    is_valid       := true;
    is_admin       := v_is_admin;
    tenant_id      := v_tenant_id;
    tenant_role    := v_tenant_role;
    write_scopes   := COALESCE(v_write_scopes, '{}'::TEXT[]);
    confirm_writes := COALESCE(v_confirm_writes, false);
    principal_id   := v_principal_id;
    RETURN NEXT;
    RETURN;
END;
$$;

-- --- ctx_auth: key_plaintext → api_key_id, delegiert an ctx_auth_by_id -------
CREATE FUNCTION ctx_auth(p_api_key TEXT)
RETURNS TABLE (
    api_key_id     UUID,
    home_scope     VARCHAR(50),
    allowed_scopes TEXT[],
    read_scopes    TEXT[],
    is_valid       BOOLEAN,
    is_admin       BOOLEAN,
    tenant_id      UUID,
    tenant_role    TEXT,
    write_scopes   TEXT[],
    confirm_writes BOOLEAN,
    principal_id   UUID
) LANGUAGE plpgsql AS $$
DECLARE
    v_api_key_id UUID;
BEGIN
    -- Nur Credential→Identität; alle Gates + der last_used_at-Write liegen in
    -- ctx_auth_by_id. Ein key_hash-Miss ergibt v_api_key_id = NULL, worauf
    -- ctx_auth_by_id die Sentinel-Antwort liefert (UPDATE matcht keine Zeile).
    SELECT context_api_keys.id INTO v_api_key_id
      FROM context_api_keys
     WHERE context_api_keys.key_hash = encode(digest(p_api_key, 'sha256'), 'hex');

    RETURN QUERY SELECT * FROM ctx_auth_by_id(v_api_key_id);
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
VALUES (95, '095_ctx_auth_principal.sql', now()) ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 095_ctx_auth_principal.sql

-- @@ ctx-fold begin 096_audit_principal_fks.sql
-- =============================================================================
-- 096_audit_principal_fks.sql — Additive Principal-Audit-FKs (OAuth-Achse 01-P3)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Fundament-Welle F3 des OAuth-Stack-Ausbaus (Plan .project/plan-oauth-2026-07-09/,
-- Design 01 §4/§8-P3, Entscheidung E1=B' Big-Bang mit ADDITIVEM Audit).
--
-- Die 12 Audit-FKs auf context_api_keys (11 Tabellen; context_secrets trägt 2)
-- BEHALTEN ihre api_key_id-/*_by-Spalte (Key-/Client-Forensik) und bekommen
-- ADDITIV eine Principal-Spalte (Person-Attribution). Kein Ersetzen — das ist
-- der B'-Kipp-Punkt aus DECISIONS.md (Forensik-Verlust bei naivem Ersetzen).
--
-- Namenskonvention: `api_key_id` → `principal_id`; `X_by` → `X_by_principal`.
-- Alle neuen Spalten UUID NULL, FK → context_principals OHNE Delete-Aktion
-- (NO ACTION) — exakt gespiegelt am Bestand: alle 12 api_key-Audit-FKs sind
-- nullable + NO ACTION (einzige Ausnahme pending_writes.api_key_id CASCADE
-- NOT NULL; die neue Spalte bleibt trotzdem nullable+NO ACTION, weil der
-- Bestand vor 096 kein Principal trägt und der Anker additiv ist).
--
-- BESTAND UNVERÄNDERT (F3-Gate): kein Backfill der Alt-Zeilen. Die historische
-- Person-Attribution bleibt jederzeit deterministisch ableitbar über den
-- erhaltenen api_key-Anker (JOIN context_api_keys k → k.principal_id, seit 094
-- NOT NULL 1:1); der NO-ACTION-FK blockt Key-Deletes solange Audit-Zeilen
-- referenzieren — kein Datenverlust-Fenster. Neue Zeilen tragen beide Anker
-- (die Go-Schreibpfade leiten die Principal-Spalte per Subquery vom acting
-- Key ab — key→principal ist funktional abhängig, ein Drift "falscher
-- Principal zum Key" ist by construction ausgeschlossen, INV-A).
--
-- context_write_log bekommt die Spalte NUR im Schema: seine beiden Writer
-- (guard, dream) sind interne Pfade ohne acting Key (api_key_id bleibt dort
-- NULL) — Vollständigkeit fürs Datenmodell + künftige key-getragene Writer.
--
-- Index: idx_access_log_principal spiegelt idx_access_log_api_key (Person-
-- Attribution-Query am Ziel-Scale); die übrigen Tabellen haben auch für die
-- api_key-Spalte keinen Index → keiner für die Principal-Spalte.
--
-- Idempotent (ADD COLUMN IF NOT EXISTS, FK per duplicate_object-Guard,
-- CREATE INDEX IF NOT EXISTS). Forward-only. Katalog-leichte DDL (nullable
-- Spalten ohne DEFAULT = kein Table-Rewrite); lock_timeout 3s (R-MIG2),
-- SET LOCAL ist transaktions-scoped (Runner wrappt jede Migration in eine tx).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- --- Spalten (12) -------------------------------------------------------------
ALTER TABLE context_access_log     ADD COLUMN IF NOT EXISTS principal_id         UUID;
ALTER TABLE context_write_log      ADD COLUMN IF NOT EXISTS principal_id         UUID;
ALTER TABLE context_pending_writes ADD COLUMN IF NOT EXISTS principal_id         UUID;
ALTER TABLE context_block_grants   ADD COLUMN IF NOT EXISTS granted_by_principal UUID;
ALTER TABLE context_chat_sessions  ADD COLUMN IF NOT EXISTS created_by_principal UUID;
ALTER TABLE context_oauth_clients  ADD COLUMN IF NOT EXISTS created_by_principal UUID;
ALTER TABLE context_projects       ADD COLUMN IF NOT EXISTS created_by_principal UUID;
ALTER TABLE context_secrets        ADD COLUMN IF NOT EXISTS created_by_principal UUID;
ALTER TABLE context_secrets        ADD COLUMN IF NOT EXISTS rotated_by_principal UUID;
ALTER TABLE context_settings       ADD COLUMN IF NOT EXISTS updated_by_principal UUID;
ALTER TABLE context_tenant_grants  ADD COLUMN IF NOT EXISTS created_by_principal UUID;
ALTER TABLE context_tenant_quota   ADD COLUMN IF NOT EXISTS updated_by_principal UUID;

-- --- FKs (12, NO ACTION wie der Bestand) --------------------------------------
DO $$
DECLARE
    spec RECORD;
BEGIN
    FOR spec IN
        SELECT * FROM (VALUES
            ('context_access_log',     'principal_id'),
            ('context_write_log',      'principal_id'),
            ('context_pending_writes', 'principal_id'),
            ('context_block_grants',   'granted_by_principal'),
            ('context_chat_sessions',  'created_by_principal'),
            ('context_oauth_clients',  'created_by_principal'),
            ('context_projects',       'created_by_principal'),
            ('context_secrets',        'created_by_principal'),
            ('context_secrets',        'rotated_by_principal'),
            ('context_settings',       'updated_by_principal'),
            ('context_tenant_grants',  'created_by_principal'),
            ('context_tenant_quota',   'updated_by_principal')
        ) AS t(tbl, col)
    LOOP
        BEGIN
            EXECUTE format(
                'ALTER TABLE %I ADD CONSTRAINT %I FOREIGN KEY (%I) REFERENCES context_principals(id)',
                spec.tbl, spec.tbl || '_' || spec.col || '_fkey', spec.col
            );
        EXCEPTION WHEN duplicate_object THEN NULL;
        END;
    END LOOP;
END $$;

-- --- Index (Spiegel von idx_access_log_api_key) --------------------------------
CREATE INDEX IF NOT EXISTS idx_access_log_principal ON context_access_log (principal_id);

INSERT INTO _migrations (version, filename, applied_at)
VALUES (96, '096_audit_principal_fks.sql', now()) ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 096_audit_principal_fks.sql

-- @@ ctx-fold begin 097_oauth_client_model.sql
-- =============================================================================
-- 097_oauth_client_model.sql — OAuth-Client-Modell: Metadaten-Spalten (02-W1)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Server-Welle C1 des OAuth-Stack-Ausbaus (Plan .project/plan-oauth-2026-07-09/,
-- Design 02 §3/§7-W1, Masterplan K1: C1 = Mig 097 — die Nummer war seit S1
-- reserviert, Lücken im Namensraum sind für den Runner regulär, vgl. 066/098).
--
-- Erweitert context_oauth_clients (Mig 023: 7 Spalten + created_by_principal
-- aus 096) um die für OAuth 2.1 nötigen Client-Constraints. Alle Neu-Spalten
-- sind additiv mit verhaltens-neutralen Defaults: der Bestands-Client behält
-- redirect_uris='{}' → die statische S2-Allowlist (oauth.go) bleibt bis zur
-- 03-Verdrahtung (S6/W03-7) die durchsetzende Instanz. Diese Welle ändert
-- KEIN Verhalten — sie liefert Daten, die 02-W2…W4 (Store/CLI, Metadata, DCR)
-- und Achse 03 (Enforcement am /authorize + /token) konsumieren.
--
--   redirect_uris   — exakte Allowlist (OAuth 2.1 §5.4, kein Wildcard/Substring)
--   scopes          — REQUESTABLE ceiling; NIE autoritativ (INV-B: der Client
--                     ist kein Autorisierungs-Gate, die Key-Autorität ist die
--                     harte Decke — client.scopes kann sie nie weiten)
--   grant_types     — erlaubte Grants (MVP: authorization_code, später +refresh)
--   response_types  — nur 'code' (OAuth 2.1, kein implicit)
--   token_endpoint_auth_method — none | client_secret_basic | client_secret_post
--                     (private_key_jwt bewusst NICHT im MVP, kein jwks-Pfad)
--   registration_source — admin | dcr | cimd (Forensik/GC-Selektor für W4b)
--   metadata        — RFC-7591-Low-Prio-Felder (client_uri, logo_uri, contacts,
--                     tos_uri, policy_uri, software_id)
--   updated_at      — Änderungs-Anker (RFC-7592-Vorbereitung, deferred)
--
-- 9. Operation: client_secret_hash bekommt DEFAULT '' — public (none)-Clients
-- aus DCR brauchen kein Secret. Ein leerer Hash ist NIE ein gültiges Secret:
-- die 03-Verdrahtung prüft token_endpoint_auth_method != 'none' VOR jedem
-- Secret-Vergleich (Design 02 §4), und heute liest kein Pfad den Hash (der
-- einzige Prüfer ValidateOAuthClientSecret ist Dead Code, inventory/02 §3).
-- Kein Backfill nötig: alle Bestands-Rows tragen bereits einen Hash-Wert.
--
-- Idempotent (ADD COLUMN IF NOT EXISTS; SET DEFAULT ist von Natur aus
-- idempotent). Forward-only; Spalten droppbar, solange kein DCR-Client
-- existiert. Katalog-leichte DDL; lock_timeout 3s (R-MIG2), SET LOCAL ist
-- tx-scoped (Runner wrappt). test.sh T07: keine neue Tabelle (bleibt 42).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE context_oauth_clients
    ADD COLUMN IF NOT EXISTS redirect_uris  text[]      NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS scopes         text[]      NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS grant_types    text[]      NOT NULL DEFAULT '{authorization_code}',
    ADD COLUMN IF NOT EXISTS response_types text[]      NOT NULL DEFAULT '{code}',
    ADD COLUMN IF NOT EXISTS token_endpoint_auth_method varchar(32) NOT NULL DEFAULT 'none',
    ADD COLUMN IF NOT EXISTS registration_source        varchar(16) NOT NULL DEFAULT 'admin',
    ADD COLUMN IF NOT EXISTS metadata       jsonb       NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS updated_at     timestamptz NOT NULL DEFAULT now();

ALTER TABLE context_oauth_clients
    ALTER COLUMN client_secret_hash SET DEFAULT '';
-- @@ ctx-fold end 097_oauth_client_model.sql

-- @@ ctx-fold begin 098_oauth_codes.sql
-- =============================================================================
-- 098_oauth_codes.sql — Persistenter OAuth-Authorization-Code-Store (03-W03-1)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Security-Welle S1 des OAuth-Stack-Ausbaus (Plan .project/plan-oauth-2026-07-09/,
-- Design 03 §3/§7-W03-1, Masterplan K1: S1 = Mig 098; 097 bleibt fuer C1/02-W1
-- reserviert — Luecken im Namensraum sind fuer den Runner regulaer, vgl. 066).
--
-- Ersetzt die prozess-lokale In-Memory-Code-Map (oauth.go): Codes ueberleben
-- Restarts und sind multi-instanz-faehig (Code auf Instanz A, /token auf B).
-- Single-use wird zum atomaren DELETE ... RETURNING (race-/HA-sicher, zwei
-- parallele /token-Calls bekommen genau eine Row).
--
--   code_hash      — SHA-256(code); der Klartext-Code wird NIE gespeichert.
--   api_key_id     — INV-A Einzel-Key-Selektor (Design 01), statt Klartext-Key.
--   principal_id   — Person-Anker (Achse 01, seit 094).
--   client_id      — nullable bis W03-2/S2 (dort wird client_id mandatory).
--   resource/scope — Schema-komplett per Design 03 §3; Befuellung ab W03-6.
--   api_key_sealed — UEBERGANGSSPALTE (W03-1→W03-3): /token liefert bis zu den
--                    opaken Tokens (W03-3/S3) weiterhin den api_key als
--                    access_token (E2-Bestandsverhalten). Der Klartext liegt
--                    hier NICHT roh, sondern AES-256-GCM-verschluesselt unter
--                    einem AUS DEM CODE abgeleiteten Schluessel — die DB kennt
--                    nur code_hash, nicht den Code: ein DB-Dump/Backup allein
--                    kann den Key nicht rekonstruieren; einloesen kann ihn nur
--                    der Code-Besitzer (der OAuth-Client am /token). W03-3
--                    droppt die Spalte ersatzlos.
--
-- ON DELETE CASCADE auf beiden FKs: ein geloeschter Key/Principal reisst seine
-- offenen Codes mit (fail-closed, keine einloesebaren Waisen-Codes).
--
-- Idempotent (CREATE TABLE/INDEX IF NOT EXISTS). Forward-only. Katalog-leichte
-- DDL; lock_timeout 3s (R-MIG2), SET LOCAL ist tx-scoped (Runner wrappt).
-- test.sh T07: +1 Tabelle (41→42).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_oauth_codes (
    id             UUID PRIMARY KEY DEFAULT uuidv7(),
    code_hash      VARCHAR(64) NOT NULL UNIQUE,
    api_key_id     UUID NOT NULL REFERENCES context_api_keys(id) ON DELETE CASCADE,
    principal_id   UUID NOT NULL REFERENCES context_principals(id) ON DELETE CASCADE,
    client_id      VARCHAR(64),
    redirect_uri   TEXT NOT NULL,
    code_challenge VARCHAR(128) NOT NULL,
    resource       TEXT,
    scope          TEXT,
    api_key_sealed BYTEA,
    expires_at     TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- GC-Sweep-Pfad (EvictExpiredOAuthCodes, Scheduler-Janitor-Tick).
CREATE INDEX IF NOT EXISTS idx_oauth_codes_expires ON context_oauth_codes (expires_at);

INSERT INTO _migrations (version, filename, applied_at)
VALUES (98, '098_oauth_codes.sql', now()) ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 098_oauth_codes.sql

-- @@ ctx-fold begin 099_access_tokens.sql
-- =============================================================================
-- 099_access_tokens.sql — Universeller opaker Token-Store (03-W03-3 / S3)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Security-Welle S3 des OAuth-Stack-Ausbaus (Plan .project/plan-oauth-2026-07-09/,
-- Design 03 §3, Masterplan K1: S3 = Mig 099; K2: E-03h/E-05-3-Reconcile —
-- DIESE Tabelle ist der EINE Universal-Credential-Store. Achse 05 baut KEINEN
-- zweiten (ihr früheres context_sessions ist verworfen, 05 §3-Kasten); sie
-- legt in Mig 102 nur den Web-Overlay context_web_sessions mit FK hierauf an.
-- Login (05) prägt issued_via='login'-Rows mit audiences=[ctx-mcp, ctx-web]
-- in DIESE Tabelle → E2 Universal-Credential strukturell (ein Token ist
-- zugleich MCP-Bearer).
--
--   token_hash     — SHA-256(opaker Token); Klartext (ctxt_/ctxr_-Präfix +
--                    32B crypto/rand hex) wird NIE gespeichert.
--   token_type     — 'access' | 'refresh' als Row-pro-Token (getrennte TTLs);
--                    Refresh-Minting kommt erst mit S4 (Rotation).
--   api_key_id     — INV-A Einzel-Key-Selektor: ein Token materialisiert
--                    IMMER genau einen Key, nie eine Key-Menge.
--   principal_id   — Person-Anker (Achse 01, seit 094).
--   audiences      — RFC 8707; enthält immer die ctx-MCP-Resource. Der
--                    Mengen-Check am /mcp kommt mit S5 (W03-6).
--   scope          — nie autoritativ (INV-B: Autorisierung = voller Key-Scope
--                    via ctx_auth_by_id; Design 03 §5).
--   refresh_family — Rotations-Familie für Reuse-Detection (S4).
--   parent_id      — rotiert-aus-Kette (S4). ON DELETE SET NULL: der GC darf
--                    Eltern-Rows räumen, ohne Kinder zu blockieren.
--   issued_via     — 'oauth' (/token) | 'login' (Achse 05) — E2-Provenienz.
--
-- Fail-closed-Auflösung (Go, resolveCredential): unbekannter Hash, expired,
-- revoked_at gesetzt, Key inaktiv, Principal inaktiv → alles 401; die
-- Materialisierung läuft über ctx_auth_by_id (095) — EIN Gate-Ort, ein
-- revozierter Key tötet seine Tokens ohne separate Token-Revocation.
--
-- Zweitens: context_oauth_codes.api_key_sealed wird ERSATZLOS gedroppt —
-- die S1-Übergangsspalte (code-versiegelter Klartext-Key für den
-- /token-Passthrough) ist mit den opaken Tokens obsolet; /token braucht nur
-- noch api_key_id. Offene Codes aus der Vor-S3-Ära verlieren ihr Blob, der
-- Flow bleibt intakt (der neue /token-Pfad liest die Spalte nicht mehr).
--
-- ON DELETE CASCADE auf key/principal: gelöschter Key/Principal reißt seine
-- Tokens mit (fail-closed, keine auflösbaren Waisen-Tokens).
--
-- Idempotent (IF [NOT] EXISTS). Forward-only. Katalog-leichte DDL;
-- lock_timeout 3s (R-MIG2), SET LOCAL ist tx-scoped (Runner wrappt).
-- test.sh T07: +1 Tabelle (44→45).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_access_tokens (
    id             UUID PRIMARY KEY DEFAULT uuidv7(),
    token_hash     VARCHAR(64) NOT NULL UNIQUE,
    token_type     VARCHAR(16) NOT NULL CHECK (token_type IN ('access', 'refresh')),
    api_key_id     UUID NOT NULL REFERENCES context_api_keys(id) ON DELETE CASCADE,
    principal_id   UUID NOT NULL REFERENCES context_principals(id) ON DELETE CASCADE,
    client_id      VARCHAR(64),
    audiences      TEXT[] NOT NULL,
    scope          TEXT,
    refresh_family UUID,
    parent_id      UUID REFERENCES context_access_tokens(id) ON DELETE SET NULL,
    issued_via     VARCHAR(16) NOT NULL CHECK (issued_via IN ('oauth', 'login')),
    expires_at     TIMESTAMPTZ NOT NULL,
    revoked_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Revoke-all über den Key (Key-Delete cascaded, Key-Soft-Revoke gated in
-- ctx_auth_by_id; der Index trägt Familien-Revokes + Audits per Key).
CREATE INDEX IF NOT EXISTS idx_access_tokens_api_key ON context_access_tokens (api_key_id);
-- GC-Sweep-Pfad (EvictExpiredOAuthTokens, Scheduler-Janitor-Tick).
CREATE INDEX IF NOT EXISTS idx_access_tokens_expires ON context_access_tokens (expires_at);
-- Familien-Revoke bei Refresh-Reuse (S4): partial auf die Refresh-Rows.
CREATE INDEX IF NOT EXISTS idx_access_tokens_family
    ON context_access_tokens (refresh_family) WHERE token_type = 'refresh';

-- S1-Übergangsspalte raus (098-Kommentar: „W03-3 droppt die Spalte ersatzlos").
ALTER TABLE context_oauth_codes DROP COLUMN IF EXISTS api_key_sealed;

INSERT INTO _migrations (version, filename, applied_at)
VALUES (99, '099_access_tokens.sql', now()) ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 099_access_tokens.sql

-- @@ ctx-fold begin 100_oauth_providers.sql
-- =============================================================================
-- 100_oauth_providers.sql — OAuth-Provider-Allowlist (OAuth-Achse 04-W1a)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Consumer-Welle L1 des OAuth-Stack-Ausbaus (Plan .project/plan-oauth-2026-07-09/,
-- Design 04 §3.2/§7-W1a, Masterplan K1: L1 = Mig 100+101; die §3-Platzhalter
-- "Mig 097" sind vom Masterplan über alle Achsen linearisiert — 097 bleibt für
-- C1/02-W1 reserviert, Lücken im Namensraum sind für den Runner regulär).
--
-- Der Allowlist-Träger für externes Login (INV-C): NUR hier registrierte
-- Identity-Provider werden vertraut. expectedIssuer kommt bei der Token-Verify
-- IMMER aus dieser Config-Zeile, NIE aus der rohen iss-Claim des Tokens —
-- Issuer-Spoofing ist damit strukturell blockiert. Anlegen ist admin-only
-- (Achse 04 W4/tierServerAdmin); ein Angreifer kann keinen eigenen IdP
-- registrieren. Reine Schema-Welle: 0 Consumer-Code aktiv, Bestands-Auth
-- bleibt byte-identisch.
--
--   slug          — stabiler Handle in URLs (/auth/login/<slug>) UND im
--                   sealbox-Secret-Namen 'oauth_provider.<slug>.client_secret'.
--                   CHECK-Regex ^[a-z0-9][a-z0-9._-]{0,40}$ hält den slug
--                   ValidSecretName-kompatibel (store/sealbox.go:27 verbietet
--                   ':' — daher Separator '.' im Secret-Namen) und schließt
--                   Pfad-/Header-Injektion über den URL-Teil aus.
--   type          — 'oidc' (Discovery + ID-Token) | 'github' (feste Endpoints,
--                   Userinfo-basiert, kein ID-Token). CHECK, fail-closed.
--   issuer        — OIDC: Discovery-Basis; github: 'https://github.com'.
--                   Der validierte Vertrauensanker für UNIQUE(issuer,subject).
--   client_id     — Client-Kennung beim externen IdP.
--                   client_secret liegt NICHT hier: at-rest verschlüsselt in
--                   context_secrets via sealbox (AES-256-GCM), Name
--                   'oauth_provider.<slug>.client_secret' (E4c). Kein
--                   Klartext-Secret in der Provider-Row, kein zweiter
--                   Krypto-Stack.
--   token_auth    — Token-Endpoint-Auth: 'client_secret_post' | '_basic' |
--                   'none' (public/native Client, RFC 7591/OAuth 2.1 — dann
--                   existiert KEIN Secret, Exchange ist PKCE-only). CHECK.
--   redirect_base — ctx-externe Origin für die redirect_uri; NULL ⇒ Fallback
--                   auf Config. Callback = <base>/auth/callback/<slug>, exakt
--                   so beim Provider registriert.
--   scopes        — angefragte OAuth-Scopes (Default OIDC-Trio).
--   auth_url/token_url/userinfo_url — nur github/non-discovery; OIDC=NULL
--                   (Endpoints kommen aus dem Discovery-Doc, dessen issuer
--                   gegen provider.issuer erzwungen wird — Substitutions-Schutz).
--   id_token_algs — erlaubte Signatur-Algorithmen (parametrisiert den
--                   RS256-Hardlock): kein 'alg:none', keine RS/HS-Confusion.
--   single_tenant_issuer / allowed_claim — INV-C-Härtung (F3): bei einem
--                   Multi-Tenant-Issuer (Azure 'common', Google ohne hd) ist
--                   iss für ALLE Orgs identisch → false erzwingt zur Laufzeit
--                   einen {claim,values[]}-Filter (tid|hd|org); fehlt er, ist
--                   der Provider inaktiv (fail-closed, Go-seitig W4/W6 —
--                   bewusst kein DB-CHECK, damit Admin-Config schrittweise
--                   entstehen darf, ohne dass eine halbe Row je vertraut wird).
--   active        — Kill-Switch pro Provider (Login lädt nur active=true).
--   created_by / created_by_principal — B'-Dual-Attribution (analog 096):
--                   Key-Forensik + Person-Anker, beide additiv-nullable.
--                   created_by ON DELETE SET NULL spiegelt den Bestand
--                   context_oauth_clients.created_by (023); der Principal-FK
--                   ohne Delete-Aktion (NO ACTION) spiegelt 096 — blockt
--                   Principal-Hard-Deletes, solange Config-Zeilen attribuieren.
--
-- Kein tenant_id/scope-Diskriminator: Provider sind wie context_oauth_clients
-- Operator-global (tenant-blind); Tenant/Rolle des eingeloggten Nutzers
-- entscheidet Achse 05, nicht der Provider.
--
-- Idempotent (CREATE TABLE IF NOT EXISTS). Forward-only, rein additiv.
-- Katalog-leichte DDL; lock_timeout 3s (R-MIG2), SET LOCAL ist tx-scoped
-- (Runner wrappt jede Migration in eine eigene Transaktion).
-- test.sh T07: +1 Tabelle (42→43; nach L1 gesamt 44 — T07-Erwartung anpassen).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_oauth_providers (
    id                   UUID PRIMARY KEY DEFAULT uuidv7(),
    slug                 VARCHAR(50) NOT NULL UNIQUE
                             CHECK (slug ~ '^[a-z0-9][a-z0-9._-]{0,40}$'),
    type                 VARCHAR(20) NOT NULL
                             CHECK (type IN ('oidc', 'github')),
    display_name         TEXT NOT NULL,
    issuer               VARCHAR NOT NULL,
    client_id            VARCHAR NOT NULL,
    token_auth           VARCHAR(20) NOT NULL DEFAULT 'client_secret_post'
                             CHECK (token_auth IN ('client_secret_post', 'client_secret_basic', 'none')),
    redirect_base        VARCHAR,
    scopes               TEXT[] NOT NULL DEFAULT '{openid,profile,email}',
    auth_url             VARCHAR,
    token_url            VARCHAR,
    userinfo_url         VARCHAR,
    id_token_algs        TEXT[] NOT NULL DEFAULT '{RS256}',
    single_tenant_issuer BOOLEAN NOT NULL DEFAULT true,
    allowed_claim        JSONB,
    active               BOOLEAN NOT NULL DEFAULT true,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by           UUID REFERENCES context_api_keys(id) ON DELETE SET NULL,
    created_by_principal UUID REFERENCES context_principals(id)
);

INSERT INTO _migrations (version, filename, applied_at)
VALUES (100, '100_oauth_providers.sql', now()) ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 100_oauth_providers.sql

-- @@ ctx-fold begin 101_sso_states.sql
-- =============================================================================
-- 101_sso_states.sql — SSO-State-Store + Identitäts-Verfeinerung (04-W1b)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Consumer-Welle L1 des OAuth-Stack-Ausbaus (Plan .project/plan-oauth-2026-07-09/,
-- Design 04 §3.3+§3.1/§7-W1b, Masterplan K1: L1 = Mig 100+101).
--
-- context_sso_states — serverseitiger, single-use Login-State (Port des
-- serviceportal-Musters sso_pending_states). Bewusst DB-basiert statt
-- in-memory (Kontrast MCP-codeStore): HA-safe by design — Login-Start auf
-- Instanz A, Callback auf Instanz B funktioniert; States überleben Restarts.
--
--   id         — opaker Cookie-Wert = Row-Lookup-Key, steht NIE in einer URL.
--                Doppel-UUID-Prinzip (F1): Row-id (im httpOnly-Cookie) und
--                OAuth-state-Param (in state_data.state) sind ZWEI getrennte
--                UUIDs. Verzehr läuft über die Cookie-Row-id, DANACH wird der
--                URL-state gegen state_data.state verglichen — kollabiert man
--                beide, landet der Consume-Key in IdP-Logs/Referrer/History
--                und der state-Check vergleicht einen Wert mit sich selbst.
--                DEFAULT uuidv7() ist Fallback; der Go-Pfad (W5/W6) erzeugt
--                die id selbst.
--   purpose    — 'login' | 'link' (Cross-Use-Schutz, F1): der Verzehr filtert
--                purpose mit, ein Login-State kann nie einen Account-Link
--                autorisieren und umgekehrt. Kein CHECK — die Werteliste ist
--                Go-seitig (W5) geschlossen, der Consume-Pfad matcht exakt.
--   state_data — {provider_slug, state, pkce_verifier, nonce, return_to}.
--                provider_slug bindet den Callback an den beim Login gewählten
--                Provider (F2, Mix-up-Abwehr): tokenURL/client_id/issuer kommen
--                aus dem versiegelten State, nie aus der Callback-URL.
--   expires_at — TTL; Verzehr ist ein atomarer
--                DELETE … WHERE id=$1 AND purpose=$2 AND expires_at>now()
--                RETURNING state_data — race-/HA-sicherer single-use, ein
--                zweiter Verzehr desselben States bekommt nichts (Replay/CSRF).
--
-- idx_sso_states_expires — GC-Sweep-Pfad für den Cleanup-Worker
-- (CleanupExpiredSSOStates, analog idx_oauth_codes_expires aus 098) —
-- unbegrenztes Wachstum durch abgebrochene Logins wird weggeräumt.
--
-- context_external_identities (aus 094) — additive Verfeinerung (§3.1):
--   last_login_at — Login-Aktivität, NICHT auth-relevant (reine Anzeige/Audit).
--   display_name  — aus dem Provider-Claim, reine Anzeige. Beide Spalten sind
--                   bewusst KEIN Vertrauensanker — der bleibt allein
--                   UNIQUE(issuer,subject) + verified_at (INV-C).
--   idx_external_identities_principal — "welche Identitäten hat Principal X"
--                   (Profil-/Linking-Ansicht, Achse 05/W7) am Ziel-Scale.
--
-- Bestands-Auth bleibt byte-identisch: neue Tabelle + zwei nullable Spalten
-- ohne DEFAULT (kein Table-Rewrite) + zwei Indexe, kein bestehender Auth-Pfad
-- liest eines davon.
--
-- Idempotent (CREATE TABLE/INDEX IF NOT EXISTS, ADD COLUMN IF NOT EXISTS).
-- Forward-only, rein additiv. Katalog-leichte DDL; lock_timeout 3s (R-MIG2),
-- SET LOCAL ist tx-scoped (Runner wrappt jede Migration in eine Transaktion).
-- test.sh T07: +1 Tabelle (43→44; T07-Erwartung 42→44 anpassen, L1 gesamt).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- --- SSO-State (single-use, HA-safe) ------------------------------------------
CREATE TABLE IF NOT EXISTS context_sso_states (
    id         UUID PRIMARY KEY DEFAULT uuidv7(),
    purpose    VARCHAR(10) NOT NULL,
    state_data JSONB NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- GC-Sweep-Pfad (CleanupExpiredSSOStates, Scheduler-Janitor-Tick).
CREATE INDEX IF NOT EXISTS idx_sso_states_expires ON context_sso_states (expires_at);

-- --- context_external_identities: additive Verfeinerung (§3.1) ----------------
ALTER TABLE context_external_identities ADD COLUMN IF NOT EXISTS last_login_at TIMESTAMPTZ;
ALTER TABLE context_external_identities ADD COLUMN IF NOT EXISTS display_name  VARCHAR;

CREATE INDEX IF NOT EXISTS idx_external_identities_principal
    ON context_external_identities (principal_id);

INSERT INTO _migrations (version, filename, applied_at)
VALUES (101, '101_sso_states.sql', now()) ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 101_sso_states.sql

-- @@ ctx-fold begin 102_web_sessions.sql
-- =============================================================================
-- 102_web_sessions.sql — Web-Overlay context_web_sessions (05-W1 / R1)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- RBAC-/Session-Welle R1 des OAuth-Stack-Ausbaus (Plan .project/plan-oauth-2026-07-09/,
-- Design 05 §3, Masterplan K1: R1 = Mig 102; K2: E-05-3 = Variante (a) —
-- eigene Overlay-Tabelle, weil csrf/ua/ip Web-only-Rauschen auf der
-- universellen Token-Tabelle wären).
--
-- Reiner Web-Overlay über dem EINEN Universal-Credential-Store
-- context_access_tokens (099): hält KEINEN Token-Klartext und KEINEN
-- Token-Hash — die Cookie-Bindung läuft über access_token_id auf die von 03
-- gehaltene, SHA-256-gehashte Token-Row. Hier liegen nur Referenzen +
-- Web-only-Daten (CSRF, Forensik).
--
--   principal_id    — Audit/whoami, NIE Autorisierungs-Eingabe (INV-B:
--                     Autorisierung = voller Key-Scope via ctx_auth_by_id).
--   access_token_id — der aktuelle ctxt_-Access-Token dieser Cookie-Session.
--                     INV-A: der Overlay verweist auf genau EINE Token-Row
--                     mit genau EINEM api_key_id (der Selektor lebt auf der
--                     Token-Row, nicht doppelt hier) — kein Feld unioniert
--                     Keys eines Principals.
--   refresh_family  — = context_access_tokens.refresh_family; die
--                     Cookie-Rotation folgt der 03-Lineage (S4): bei Refresh
--                     rotiert 03 die Token-Rows innerhalb der Familie, der
--                     Overlay zeigt access_token_id auf die neue Row um. In
--                     099 ist die Spalte nullable (Access-Rows ohne Refresh-
--                     Lineage existieren dort); HIER NOT NULL — eine
--                     Web-Session existiert nur mit Refresh-Lineage, das
--                     CSRF-Secret hängt an der Familie, nicht am Access-Token.
--   csrf_secret     — per-Session Synchronizer-Token (05 §4.4), server-seitig
--                     gehalten, bleibt über Token-Rotationen stabil.
--   user_agent /
--   client_ip       — Forensik (optional), reine Anzeige/Audit.
--
-- Instant-Revoke läuft NICHT über FK-CASCADE: der reguläre Key-Revoke ist
-- Soft-Delete (context_api_keys.active=false), die Overlay-Row überlebt ihn —
-- der per-Request-Resolver (resolveCredential → ctx_auth_by_id) re-appliziert
-- die active-/Status-Gates und fällt fail-closed auf 401 (05 §4.1). Die
-- ON DELETE CASCADEs hier sind nur Aufräum-Netz für die seltenen Hard-Deletes
-- (Tenant-Offboarding, Tests) — keine auflösbaren Waisen-Overlays.
--
-- KEINE Go-Änderung in dieser Welle; Store-Funktionen kommen mit R2.
--
-- Idempotent (CREATE TABLE/INDEX IF NOT EXISTS). Forward-only, rein additiv.
-- Katalog-leichte DDL; lock_timeout 3s (R-MIG2), SET LOCAL ist tx-scoped
-- (Runner wrappt jede Migration in eine Transaktion).
-- test.sh T07: +1 Tabelle (45→46), col_count context_blocks BLEIBT 40.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE TABLE IF NOT EXISTS context_web_sessions (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    principal_id    UUID NOT NULL REFERENCES context_principals(id) ON DELETE CASCADE,
    access_token_id UUID NOT NULL REFERENCES context_access_tokens(id) ON DELETE CASCADE,
    refresh_family  UUID NOT NULL,
    csrf_secret     TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at    TIMESTAMPTZ,
    user_agent      TEXT,
    client_ip       INET
);

-- Rotation folgen / Familie invalidieren (Refresh-Reuse → Familien-Revoke, S4).
CREATE INDEX IF NOT EXISTS idx_web_sessions_family ON context_web_sessions (refresh_family);
-- „meine aktiven Browser-Sessions"-Liste (Profil-Ansicht, 05/W7) am Ziel-Scale.
CREATE INDEX IF NOT EXISTS idx_web_sessions_principal ON context_web_sessions (principal_id);

INSERT INTO _migrations (version, filename, applied_at)
VALUES (102, '102_web_sessions.sql', now()) ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 102_web_sessions.sql

-- @@ ctx-fold begin 103_audit_trail_structural_references.sql
-- =============================================================================
-- 103_audit_trail_structural_references.sql — audit-trail: references link class
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- The daily synthesis report (dream/synthesize_report.go) enumerates the blocks
-- it summarizes (fetchDailyNewBlocks) and — since this wave — persists that
-- fact as deterministic context_structural_links edges (link_class=references,
-- origin=system): report → source block. The write path validates the class
-- against the source type's structural_link_classes allowlist (design/02 §4.1,
-- fail-closed: a type that declares none permits no structural links), and
-- reports classify as audit-trail (metadata.source=dream-synthesis matches the
-- source_prefixes rule). This migration declares the class on the type.
--
-- Deliberate UPDATE of the builtin _global audit-trail row in lockstep with
-- internal/blocktype/builtin.go (085 pattern — the golden gate
-- registry_integration_test.go::TestRegistryGolden_Integration diffs the
-- decoded DB row against the compiled-in builtin set; both move together or it
-- goes red). All other fields stay byte-identical to the 072 seed. Tenant-
-- scoped overrides shadow the _global row via a separate row and are untouched.
-- Idempotent (fixed target values); a second run is a no-op write.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

UPDATE context_block_types
SET config = '{
  "v": 1,
  "retrieval": {"policy": "damped", "damping_factor": 0.3,
                "intent_patterns": ["session","welle","audit","recurrent","handover",
                                    "self-audit","dream v","performance","reset","baseline"]},
  "guard":     {"check": true, "candidate": true},
  "dream":     {"linkable": true},
  "digest":    {"include": true},
  "overview":  {"include": true},
  "parent":    {"mode": "none"},
  "structural_link_classes": ["references"],
  "classify":  {"priority": 20,
                "source_prefixes": ["dream-"],
                "title_patterns": ["session","welle","audit","recurrent","handover",
                                   "self-audit","dream v","performance","reset","baseline"]}
}'::jsonb
WHERE name = 'audit-trail' AND scope = '_global' AND builtin = true;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (103, '103_audit_trail_structural_references.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 103_audit_trail_structural_references.sql

-- @@ ctx-fold begin 104_struct_links_graph_traversal.sql
-- =============================================================================
-- 104_struct_links_graph_traversal.sql — Ego-Traversal-Indizes (graph-structural GB1)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- M050 pattern (050:33-42) carried onto context_structural_links: seed column
-- leading, created_at DESC as the second key column (= free per-leg ordering,
-- Merge Append + early termination at the per-node LIMIT), INCLUDE payload
-- saves the heap fetch on the link row. created_at is the ordering-key stand-in
-- for the missing confidence column (conf 1.0 by definition, 076:8-9): "newest
-- reference wins the cap slot" (E5) — stable because PutStructuralLink
-- ON CONFLICT DO NOTHING never bumps created_at.
-- NO partial WHERE (unlike M050): structural has no supersedes.
--
-- Index balance after M104 = 4 B-trees (PK, idx_struct_links_scope,
-- graph_fwd, graph_rev; idx_struct_links_rev retires below, K4). Write amplification is
-- accepted for v1: the expected forge-sync bulk import is a batch/background
-- path, and ON CONFLICT DO NOTHING rejects re-puts without touching indexes.
-- CREATE INDEX without CONCURRENTLY is a no-op lock at today's volume; a
-- LATER re-deploy against a million-row table is caught by lock_timeout (the
-- migration fails instead of blocking) — CONCURRENTLY out-of-band is the
-- documented escape route (design/01 §3.2).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE INDEX IF NOT EXISTS idx_struct_links_graph_fwd
    ON context_structural_links (source_block_id, created_at DESC)
    INCLUDE (target_block_id, link_class, origin);

CREATE INDEX IF NOT EXISTS idx_struct_links_graph_rev
    ON context_structural_links (target_block_id, created_at DESC)
    INCLUDE (source_block_id, link_class, origin);

-- K4 consolidation (design/01 §3.2, decided by the GB1 EXPLAIN gate):
-- idx_struct_links_rev (076: target, link_class INCLUDE source) retires —
-- graph_rev above is also target-leading and serves its ONLY consumer (the
-- StructuralNeighbors reverse leg, structlinks.go) as an index scan with
-- link_class as a filter. Drop-probe evidence lives in
-- TestStructuralHop_IndexPlan (asserted, not just logged). Index balance
-- stays at 4 B-trees instead of 5.
DROP INDEX IF EXISTS idx_struct_links_rev;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (104, '104_struct_links_graph_traversal.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 104_struct_links_graph_traversal.sql

-- @@ ctx-fold begin 105_structural_class_collision_preflight.sql
-- =============================================================================
-- 105_structural_class_collision_preflight.sql — dream/structural namespace
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Companion to the reservedGraphClasses guard (graph-structural GA6/GA7,
-- design/02 §4.5): DecodePolicy now REJECTS the five dream relationship names
-- inside structural_link_classes — on the write path AND on every registry
-- (re)load. The guard therefore acts retroactively on already-persisted rows,
-- and the degradation direction is not neutral: a pre-existing colliding BASE
-- row would fail the whole base reload into the builtin fallback (documented
-- FAIL-OPEN — operator visibility narrowings revert), and a colliding TENANT
-- row collapses that tenant's overlay onto base.
--
-- This preflight makes the deploy stop LOUDLY before the binary switch instead
-- of poisoning reloads silently: it RAISEs with row coordinates if
--   (a) any context_block_types config declares a reserved dream class in
--       structural_link_classes, or
--   (b) any context_structural_links row carries a dream class as link_class
--       (raw-SQL legacies: the manage write path validates via the registry,
--       raw SQL does not; 076 deliberately has no CHECK).
-- Read-only checks — idempotent by construction; a clean corpus is a no-op.
-- Scope note (E15): arm (a) reads the current STRING-form entries of
-- structural_link_classes; a future object form ({"class":...}) yields JSON
-- text here that never matches — by then the DecodePolicy guard (the durable
-- enforcement, GA6) must carry the reservation, not this one-shot preflight.
-- The live corpus was sweep-verified collision-free on 2026-07-14 (builtin:
-- references, duplicate-of), so this migration documents + enforces, it does
-- not migrate data.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

DO $$
DECLARE bad RECORD;
BEGIN
  SELECT t.name, t.scope, c.class
    INTO bad
    FROM context_block_types t,
         LATERAL jsonb_array_elements_text(
           COALESCE(t.config->'structural_link_classes', '[]'::jsonb)
         ) AS c(class)
   WHERE c.class IN ('topical','factual','causal','recurrent','supersedes')
   LIMIT 1;
  IF FOUND THEN
    RAISE EXCEPTION USING MESSAGE = format(
      'structural-class collision preflight (105): block type %I (scope %I) declares reserved dream class %I in structural_link_classes — fix the row before deploying the namespace guard (a colliding row would poison every registry reload into the fail-open builtin fallback)',
      bad.name, bad.scope, bad.class);
  END IF;
END $$;

DO $$
DECLARE bad RECORD;
BEGIN
  SELECT l.source_block_id, l.target_block_id, l.link_class, l.scope
    INTO bad
    FROM context_structural_links l
   WHERE l.link_class IN ('topical','factual','causal','recurrent','supersedes')
   LIMIT 1;
  IF FOUND THEN
    RAISE EXCEPTION USING MESSAGE = format(
      'structural-class collision preflight (105): context_structural_links row %s -> %s (scope %I) carries dream class %I as link_class — raw-SQL legacy, delete or reclass the row before deploying (076 has no CHECK; the vocabulary split would route it ambiguously)',
      bad.source_block_id, bad.target_block_id, bad.scope, bad.link_class);
  END IF;
END $$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (105, '105_structural_class_collision_preflight.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 105_structural_class_collision_preflight.sql

-- @@ ctx-fold begin 106_graph_stat_indexes.sql
-- =============================================================================
-- 106_graph_stat_indexes.sql — statistics/prune indexes for both link tables
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Plan graph-structural 2026-07-14, wave GD1 (design/04 §3, W04-4). Two
-- indexes, built NOW and in FINAL form while the tables are small: the
-- single-tx migration runner cannot CREATE INDEX CONCURRENTLY (N11 doctrine,
-- store/tenant.go), so a later build or shape change at 500k/3M rows would
-- lock the table for the build duration.
--
-- (1) idx_struct_links_scope_created — daily-report window statistics (GD2):
--     count per (link_class, origin) over [from, to) within the own scope.
--     The existing idx_struct_links_scope (076) covers only the scope column;
--     every window aggregation would heap-fetch all tenant rows. COVERING
--     (INCLUDE link_class, origin) because the query selects both: with the
--     INCLUDE the count is a true index-only scan (insert-only table —
--     PG13+ insert-triggered autovacuum keeps the visibility map fresh).
--     idx_struct_links_scope stays (dropping a live index is its own risk);
--     the fourth index's write amplification is µs-range for the three
--     writers (daily report, forge-sync bursts, manage API).
--
-- (2) idx_dream_links_scope_created — dream side of the same statistics
--     surface (GD3, scope-filtered window count) AND the N11 prune seam
--     (store/tenant.go names exactly this missing index for 1M+ prunes).
--     The prune rationale stands independently of GD3, so the index is
--     unconditional. Deliberately NOT covering: context_dream_links is
--     replace-swept on the hottest link-write path — an INCLUDE would be
--     write amplification for a daily heap-fetch of a few hundred rows.
--
-- Idempotent (IF NOT EXISTS + ON CONFLICT footer), pure indexes: no backfill,
-- no data change. Rollback = DROP INDEX. Deployable without an app change.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE INDEX IF NOT EXISTS idx_struct_links_scope_created
    ON context_structural_links (scope, created_at)
    INCLUDE (link_class, origin);

CREATE INDEX IF NOT EXISTS idx_dream_links_scope_created
    ON context_dream_links (scope, created_at);

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (106, '106_graph_stat_indexes.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 106_graph_stat_indexes.sql

-- @@ ctx-fold begin 107_checkpoint_type_seed.sql
-- =============================================================================
-- 107_checkpoint_type_seed.sql — checkpoint block-type seed + evidence repair
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Structural anchor for ID-referenced evidence chains (compaction-checkpoint
-- manifests + transcript source parts). Root cause 2026-07-20 (learnings block
-- 019f7c7a-e6b0-7271-9717-358b460fbd27): the writer sets no type, the blocks
-- ran auto-typed "knowledge" through the DEFAULT guard lane (archive mode,
-- 0.98/0.92) — and consecutive checkpoints of one session are near-duplicates
-- BY CONSTRUCTION (manifest = boilerplate + IDs; parts overlap in the
-- transcript window). The guard auto-archived manifests/parts, and since every
-- read path filters NOT is_archived, ID reference chains broke silently
-- (dangling manifest pointer in an active compaction summary).
--
-- The checkpoint type keeps evidence blocks out of EVERY autonomous pipeline:
--   * retrieval=excluded — resolution runs exclusively over exact block IDs
--     (manifest carries source_block_ids + parent_manifest chain); in
--     retrieval the token-dense transcript parts flood candidate sets and
--     overflow the reranker slot window (1024-token slots, hex-dense prefixes
--     tokenize at ~2.1 bytes/token — the 2026-07-15 exceed_context_size_error
--     incidents).
--   * guard.check=false AND guard.candidate=false — never guard-checked,
--     never a match candidate for other blocks.
--   * dream/digest/overview=false — no links, no topic-map, no clustering.
--   * classify: stable writer title prefix "Compaction source" (priority 30,
--     after system-meta 10 / audit-trail 20). Writers SHOULD still set
--     type=checkpoint explicitly (type_source='manual').
--
-- The config MUST decode byte-equivalently to the compiled-in builtin set in
-- internal/blocktype/builtin.go (checkpoint entry): the golden integration
-- test applies THIS file from migrations.FS and diffs the decoded rows against
-- the builtin set (drift gate, design/01 §4.1 R1). ON CONFLICT DO NOTHING
-- keeps the seed idempotent and never overwrites operator tuning on re-run.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config) VALUES
('checkpoint', '_global', 'Checkpoint', true, false, '{
  "v": 1,
  "retrieval": {"policy": "excluded"},
  "guard":     {"check": false, "candidate": false},
  "dream":     {"linkable": false},
  "digest":    {"include": false},
  "overview":  {"include": false},
  "parent":    {"mode": "none"},
  "classify":  {"priority": 30, "title_patterns": ["compaction source"]}
}'::jsonb)
ON CONFLICT (name, scope) DO NOTHING;

-- ── Data repair (fresh DB: both statements are no-ops) ───────────────────────

-- Retype the existing checkpoint corpus. Only auto-typed rows: a manual type
-- assertion is never overridden (T4 semantics). type_source stays 'auto' —
-- the classify hook would produce the same verdict from the title pattern.
UPDATE context_blocks
   SET type_name = 'checkpoint'
 WHERE category = 'compaction-checkpoints'
   AND type_source = 'auto'
   AND type_name <> 'checkpoint';

-- Un-archive the guard-auto-archived evidence blocks (guard_status =
-- 'archived_dup') so ID reference chains resolve again. Guard metadata
-- (guard_matched_id, guard_similarity, guard_checked_at) stays intact for
-- auditability. Re-archive is impossible on both axes: the sweep predicate
-- selects only guard_checked_at IS NULL (kept set), and checkpoint is no
-- longer in the guard.check type allowlist at all.
--
-- Two guards against the partial unique index (category,title,scope)
-- WHERE NOT is_archived:
--   * rn=1 — among archived title-twins only the NEWEST row (uuidv7 order)
--     returns; an older byte-twin stays archived as a true duplicate.
--   * NOT EXISTS — never un-archive into a slot a live row already occupies.
WITH ranked AS (
  SELECT id,
         row_number() OVER (PARTITION BY category, title, scope ORDER BY id DESC) AS rn
    FROM context_blocks
   WHERE category = 'compaction-checkpoints'
     AND is_archived
     AND guard_status = 'archived_dup'
)
UPDATE context_blocks b
   SET is_archived = false,
       guard_status = 'needs_review',
       updated_at = now()
  FROM ranked r
 WHERE b.id = r.id
   AND r.rn = 1
   AND NOT EXISTS (
     SELECT 1 FROM context_blocks live
      WHERE live.category = b.category
        AND live.title = b.title
        AND live.scope = b.scope
        AND NOT live.is_archived
   );

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (107, '107_checkpoint_type_seed.sql', now())
  ON CONFLICT (version) DO NOTHING;
-- @@ ctx-fold end 107_checkpoint_type_seed.sql

-- @@ ctx-fold begin 108_migrations_checksum.sql
-- 108_migrations_checksum.sql
-- Contract-Achse W03-1: pin the applied SQL artifact (Konzept: pgContext
-- Contract-Registry, 00b §1 — Clean-Room). NULL = vor-108 appliziert;
-- der Boot-Backfill stempelt den Hash des EINGEBETTETEN Files nach.
ALTER TABLE _migrations ADD COLUMN IF NOT EXISTS checksum CHAR(64);
COMMENT ON COLUMN _migrations.checksum IS
  'sha256(hex) of the embedded migration file at record/backfill time; W11: backfill attests the present file, not the historic apply';
-- @@ ctx-fold end 108_migrations_checksum.sql

-- @@ ctx-fold begin 109_embed_provenance.sql
-- 109_embed_provenance.sql
-- Achse 04 W04-1: repariert die tote Provenienz-Spur, BEVOR die
-- Re-Embed-Statemachine (W04-3+) auf ihr aufbaut.

-- (1) embed_model: Default fällt — der Wert wird ab jetzt vom Writer gesetzt.
--     Semantik neu: "Modell-String, der den AKTUELLEN Vektor erzeugt hat; NULL = kein Vektor".
ALTER TABLE context_blocks ALTER COLUMN embed_model DROP DEFAULT;

-- (2) Bestandsdaten-Backfill. Ehrlichkeits-Fußnote: das Label behauptet
--     Raum-Zugehörigkeit unter der Äquivalenz-Annahme des Engine-Wechsels
--     2026-06-10 (Ollama->llama.cpp, gleiches Modell Q4_K_M, kein Re-Embed).
--     Vektoren von davor stammen von einem Backend mit String
--     'qwen3-embedding:8b'; die Annahme "gleicher Raum" ist seither
--     Betriebsgrundlage und wird hier fortgeschrieben, nicht neu bewiesen.
--     Deploy-Zeitpunkt-Annahme: zwei Full-Table-UPDATEs in einer Tx sind beim
--     Klein-Korpus (~1,8k Zeilen) trivial; fällt das Deploy-Fenster wider
--     Erwarten erst bei >=1M Zeilen, MÜSSEN diese UPDATEs vorher auf
--     Batch-UPDATEs (z.B. 50k-Chunks per ctid-Range, eigene Txen) umgeschnitten
--     werden.
UPDATE context_blocks SET embed_model = 'qwen3-embedding-8b' WHERE embedding IS NOT NULL;
UPDATE context_blocks SET embed_model = NULL                 WHERE embedding IS NULL;

-- (3) Tote Objekte: embed_status (nie von Go-Code beschrieben; DDL-Default
--     'done' seit 001) und sein Partial-Index.
DROP INDEX IF EXISTS idx_embed_pending;
ALTER TABLE context_blocks DROP COLUMN IF EXISTS embed_status;

-- (4) Tragender Pending-Index für beide Backfill-Pfade + künftigen
--     Migrations-Scan (K2: dieser Index gehört Achse 04):
CREATE INDEX IF NOT EXISTS idx_embedding_pending
    ON context_blocks (created_at)
    WHERE embedding IS NULL AND NOT is_archived;
-- @@ ctx-fold end 109_embed_provenance.sql

-- @@ ctx-fold begin 110_recall_runs.sql
-- 110_recall_runs.sql
-- Achse 01 W01-1: recall_check-Messläufe — ANN (HNSW, produktionsidentische
-- Prädikate bis auf den Grant-Arm) vs. exakte Brute-Force-Referenz,
-- scope-geschichtet. Aggregate only — die Tabelle darf nie Query-Texte,
-- Block-IDs oder Vektoren tragen (Leak-Schutz = Allowlist-Assertion im
-- Insert-Pfad, fail-closed). scope ist hier Mess-Objekt-Bezeichner (welcher
-- Scope wurde vermessen), kein Sichtbarkeits-Diskriminator — die Tabelle ist
-- ausschließlich server-admin-sichtbar. Kein Backfill-Pfad: nötig ist keiner,
-- erlaubt ist keiner — historische Rankings aus context_access_log wären
-- ANN-gegen-sich-selbst (kein Brute-Force-Referenzleg existierte je) und
-- damit als Recall-Backfill Scheindaten.

CREATE TABLE IF NOT EXISTS context_recall_runs (
    id              UUID PRIMARY KEY DEFAULT uuidv7(),
    ran_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    run_group       UUID NOT NULL,            -- ein Scheduler-Lauf = eine Gruppe über alle Strata
    stratum         TEXT NOT NULL,            -- 'small' | 'medium' | 'large' | 'all'
    scope           VARCHAR(50),              -- vermessener Scope; NULL beim Pseudo-Stratum 'all'
    corpus_embedded INT  NOT NULL,            -- sichtbare embeddete Blöcke im Mess-Fenster
    k               SMALLINT NOT NULL,        -- 10 und 75 (zwei Zeilen pro Probe-Satz)
    n_queries       SMALLINT NOT NULL,        -- tatsächlich gemessene Queries (nach Sampling/Budget)
    query_source    TEXT NOT NULL,            -- 'log' | 'loo' | 'mixed'
    ef_search       INT  NOT NULL,            -- effektiver Wert des ANN-Legs (0 = Default 40)
    iterative_scan  TEXT NOT NULL,            -- 'off' | 'strict_order' | 'relaxed_order'
    valid           BOOLEAN NOT NULL,         -- false => Plan-Assertion verletzt / Budget-/Demand-Abbruch
    recall_avg      DOUBLE PRECISION,         -- NULL wenn NOT valid
    recall_min      DOUBLE PRECISION,         -- schlechtester Einzel-Query-Wert (Frühwarnung)
    ann_ms_p50      DOUBLE PRECISION,
    ann_ms_p95      DOUBLE PRECISION,
    exact_ms_p50    DOUBLE PRECISION,
    meta            JSONB NOT NULL DEFAULT '{}'::jsonb
    -- meta-Schlüssel (kanonische ALLOWLIST): pgvector_version, pg_version,
    -- index_reloptions, embed_model, invalid_reason, budget_exhausted,
    -- strata_bounds, epsilon, n_eff_min, exact_touch_bytes,
    -- buffercache_delta, scope_changed. Jeder andere Schlüssel = Test ROT.
    -- Werte: nur Skalare/kurze Strings. NIE: query_text, block_ids, vectors.
);

CREATE INDEX IF NOT EXISTS idx_recall_runs_ran_at
    ON context_recall_runs (ran_at DESC);
CREATE INDEX IF NOT EXISTS idx_recall_runs_stratum
    ON context_recall_runs (stratum, scope, k, ran_at DESC); -- Status-Read: latest je (stratum,scope,k)
-- @@ ctx-fold end 110_recall_runs.sql

-- @@ ctx-fold begin 111_stratify_covering_index.sql
-- 111_stratify_covering_index.sql
-- Achse 01 W01-1b (K3): partieller Covering-Index für die
-- Stratifizierungs-/loo-Zugriffe der recall_check-Probe (Achse 01) und —
-- read-only mitgenutzt — die künftige Kardinalitäts-Schätzung des
-- Strategy-Selektors (Achse 02). Ohne ihn ist die per-Scope-Zählung
-- embeddeter aktiver Blöcke am Ziel-Scale ein Full-Heap-Scan
-- (~14 GB @10M); mit ihm ein Index-Only-Scan über (scope, type_name).
-- Eigentum: Achse 01 (K3) — Achse 02 legt KEINEN eigenen Index an.

CREATE INDEX IF NOT EXISTS idx_blocks_stratify_covering
    ON context_blocks (scope, type_name)
    WHERE NOT is_archived AND embedding IS NOT NULL;
-- @@ ctx-fold end 111_stratify_covering_index.sql

-- @@ ctx-fold begin 112_rrf_gen15_dual_arm.sql
-- =============================================================================
-- 112_rrf_gen15_dual_arm.sql — ctx_rrf Generation 15: Dual-Arm-semantic-CTE
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Achse 02 W02-1 (Evokoa-Clean-Room, design/02-strategy-selektor.md §3.2/§4.4).
-- Die 15. ctx_rrf-Generation: der semantic-Kanal wird zum Dual-Arm — der
-- heutige HNSW-Pfad (`ann`) und ein exakter Materialisierungs-Arm (`exact`)
-- für kleine Scopes (Recall 1,0, deterministische Ränge). Drei neue
-- Parameter am Ende, alle defaulted:
--
--   p_semantic_mode  TEXT    DEFAULT 'ann'   — 'ann' | 'exact'; alles andere
--                                              → RAISE (fail-closed, §5.2).
--   p_scan_tuples    INTEGER DEFAULT NULL    — >0 = SET LOCAL
--                                              hnsw.max_scan_tuples (nur im
--                                              ann-Arm, Grauzonen-Budget);
--                                              ≤0 oder >200000 → RAISE.
--                                              Obergrenze 200000 = SQL-seitige
--                                              letzte Verteidigungslinie;
--                                              Wert synchron zum Go-Clamp
--                                              (§5.4, W02-2) halten.
--   p_exact_cap      INTEGER DEFAULT NULL    — Pflicht im exact-Modus:
--                                              konstruktiver Deckel des
--                                              exakt-Arms (LIMIT) + In-Body-
--                                              Wächter (§5.6); im ann-Modus
--                                              ignoriert.
--
-- ABWÄRTSKOMPATIBILITÄT BY DEFAULT: der bestehende 15-Argument-Call
-- (rrf/search.go) läuft über die Parameter-Defaults semantisch identisch
-- weiter — Migration und Go-Binary sind entkoppelt deploybar (der Selektor
-- kommt erst mit W02-2 in rrf.Search).
--
-- Mechanik des Dual-Arms (ein Statement, kein dupliziertes RETURN-QUERY-Paar
-- — Anti-Divergenz-Entscheid §5.1):
--
--   * One-Time-Filter-Gating: `p_semantic_mode = '…'` referenziert nur einen
--     Parameter — der Planner hebt es als gating qual über den Scan; der
--     inaktive Arm wird zur Ausführungszeit komplett übersprungen (auch im
--     generischen Plan des PL/pgSQL-Plan-Caches).
--   * MATERIALIZED-Barriere: der exakt-Arm sortiert über eine materialisierte
--     Pool-CTE ohne Index — der HNSW-Index ist für ihn strukturell
--     unerreichbar, nicht bloß unattraktiv (robust gegen künftige
--     Planner-/pgvector-Kostenmodell-Änderungen). Die Pool-CTE berechnet die
--     Distanz beim Materialisieren und trägt nur (id, dist) ≈ ~16 B/Zeile
--     statt ~2 KB halfvec.
--   * Cap-Wächter (TOCTOU, §5.6): Go-Probe und dieser Call sind getrennte
--     Transaktionen ohne gemeinsamen Snapshot — wächst der Scope im Fenster
--     dazwischen (Un-Archive-Sweep, Bulk-Restore), wiederholt der exact-Zweig
--     die gedeckelte Probe in derselben Transaktion (Index
--     idx_context_scope_active, ≤ p_exact_cap+1 Einträge) inklusive
--     Grant-Addition und failt laut: ERRCODE 54000 (program_limit_exceeded);
--     Go degradiert auf ann (W02-2). `LIMIT p_exact_cap` auf der Pool-CTE
--     bleibt als konstruktive Schranke fürs Rest-Race intra-Call.
--   * Deterministischer Tiebreak (dist, id) NUR im exakt-Arm — macht ihn als
--     Ground-Truth-Leg für Achse 01 reproduzierbar. Der ann-Arm bleibt
--     bewusst ohne Tiebreak (Ist-Verhalten, approximativ per Definition).
--   * `embedding IS NOT NULL` NUR im exakt-Arm (deklariertes Semantik-Delta 2,
--     §4.5): der ann-Arm behält die Ist-Semantik der Gen 14 — bei
--     Seq-Scan-Plänen (kleine Kardinalität) erhalten NULL-Embedding-Zeilen
--     dort heute Ränge (NULLS LAST). Eine Angleichung bräche die
--     G1-Default-Kompatibilität und ist bewusst NICHT Teil dieser Generation.
--
-- INVARIANTE Nr. 1 (§5.1): beide Arme tragen den WÖRTLICH identischen
-- Sichtbarkeits-Prädikat-Block inklusive Klammerungs-Invariante (archived +
-- beide Type-Konjunkte strikt VOR der (scope OR grant)-Klammer, 073-Header).
-- Jede künftige Prädikat-Änderung MUSS beide Arme treffen — der
-- Paritäts-Sentinel (Gate W02-1-G2) reißt sonst.
--
-- `SET LOCAL`-Reichweite (dokumentierte Falle, wie 073): gilt bis
-- Transaktionsende. Heute ist der Call eine implizite
-- Single-Statement-Transaktion — die GUCs sterben mit dem Statement. Bettet
-- je ein Caller ctx_rrf in eine längere Transaktion ein, erben
-- Folge-Statements relaxed_order/max_scan_tuples.
--
-- fulltext_de/en, trigram_title, block_mass, type_factor, rrf-Fusion und
-- finale Projektion: byte-identisch zu Gen 14 (073).
--
-- Function-only, kein Tabellen-/Spalten-Change: test.sh-T07-Zähler
-- unverändert. Idempotent (DROP IF EXISTS beider Signaturen + CREATE OR
-- REPLACE), forward-only. Registrierung übernimmt der Runner
-- (store.RunMigrations, 108+-Konvention).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- Drop der Gen-14-Signatur (15 Parameter) und der neuen 18-Parameter-Signatur
-- (idempotente Re-Runs) — 048/068/073-Muster gegen 42725-Overload-Ambiguität.
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT, TEXT[], TEXT[], DOUBLE PRECISION[], TEXT[], TEXT[], UUID[]);
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT, TEXT[], TEXT[], DOUBLE PRECISION[], TEXT[], TEXT[], UUID[], TEXT, INTEGER, INTEGER);

CREATE OR REPLACE FUNCTION ctx_rrf(
    p_embedding             halfvec(1024),
    p_query                 TEXT,
    p_query_spaced          TEXT,
    p_scopes                TEXT[],
    p_category              TEXT DEFAULT NULL,
    p_tags                  TEXT[] DEFAULT NULL,
    p_limit                 INT DEFAULT 5,
    p_temporal              TEXT DEFAULT NULL,
    p_query_or              TEXT DEFAULT NULL,
    p_types_visible         TEXT[] DEFAULT NULL,   -- ALLOWLIST (fail-closed): NULL/leer ⇒ 0 Treffer
    p_damped_types          TEXT[] DEFAULT NULL,   -- parallel zu p_damped_factors
    p_damped_factors        DOUBLE PRECISION[] DEFAULT NULL,
    p_categories_exclude    TEXT[] DEFAULT NULL,
    p_types_exclude         TEXT[] DEFAULT NULL,   -- Request-Level-Exclude
    p_granted_block_ids     UUID[] DEFAULT NULL,
    p_semantic_mode         TEXT DEFAULT 'ann',    -- 'ann' | 'exact' (Gen 15, §3.2)
    p_scan_tuples           INTEGER DEFAULT NULL,  -- Grauzonen-Budget, nur ann-Arm
    p_exact_cap             INTEGER DEFAULT NULL   -- Pflicht-Deckel im exact-Modus
) RETURNS TABLE (
    rrf_score   DOUBLE PRECISION,
    cosine_sim  DOUBLE PRECISION,
    id          UUID,
    title       TEXT,
    category    VARCHAR(100),
    tags        TEXT[],
    content     TEXT,
    scope       VARCHAR(50),
    updated_at  TIMESTAMPTZ,
    type_name   TEXT
) LANGUAGE plpgsql AS $$
DECLARE
    v_n BIGINT;
BEGIN
    -- Validierung als erste Anweisungen, vor jeder GUC-Setzung (§5.2):
    -- unbekannter Mode fällt NIE still auf ann.
    IF p_semantic_mode NOT IN ('ann', 'exact') THEN
        RAISE EXCEPTION 'ctx_rrf: unknown semantic mode %', p_semantic_mode;
    END IF;
    -- Obergrenze 200000 = letzte Verteidigungslinie im SQL-Body; Wert
    -- synchron zum Go-Clamp (§5.4, W02-2) halten.
    IF p_scan_tuples IS NOT NULL AND (p_scan_tuples <= 0 OR p_scan_tuples > 200000) THEN
        RAISE EXCEPTION 'ctx_rrf: invalid scan tuples budget %', p_scan_tuples;
    END IF;

    IF p_semantic_mode = 'exact' THEN
        IF p_exact_cap IS NULL OR p_exact_cap <= 0 THEN
            RAISE EXCEPTION 'ctx_rrf: exact mode requires positive p_exact_cap';
        END IF;
        -- Cap-Wächter (§5.6): in-Tx-Wiederholung der gedeckelten Probe
        -- (Index idx_context_scope_active) + Grant-Addition. Erkennt
        -- Scope-Wachstum zwischen Go-Probe und Ausführung (TOCTOU).
        SELECT count(*) INTO v_n FROM (
            SELECT 1 FROM context_blocks cb
            WHERE cb.scope = ANY(p_scopes) AND NOT cb.is_archived
            LIMIT p_exact_cap + 1
        ) t;
        IF v_n + COALESCE(array_length(p_granted_block_ids, 1), 0) > p_exact_cap THEN
            RAISE EXCEPTION 'ctx_rrf: exact_cap_hit (cap=%)', p_exact_cap
                USING ERRCODE = '54000';  -- program_limit_exceeded; Go-Retry als ann (§5.6)
        END IF;
    END IF;

    IF p_semantic_mode = 'ann' THEN
        SET LOCAL hnsw.iterative_scan = 'relaxed_order';  -- Nachfolger der unkonditionalen 073-Zeile
        IF p_scan_tuples IS NOT NULL THEN
            PERFORM set_config('hnsw.max_scan_tuples', p_scan_tuples::text, true);  -- true = SET LOCAL
        END IF;
    END IF;

    RETURN QUERY
    WITH semantic_ann AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
            ) AS rank,
            1.0 - (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS cos_sim
        FROM context_blocks cb
        WHERE p_semantic_mode = 'ann'          -- One-Time-Filter-Gate (nur Params → gating qual)
          AND NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
        ORDER BY cb.embedding::halfvec(1024) <=> p_embedding
        LIMIT 75
    ),
    exact_pool AS MATERIALIZED (               -- Materialisierungs-Barriere: HNSW strukturell unerreichbar
        SELECT
            cb.id,
            (cb.embedding::halfvec(1024) <=> p_embedding)::DOUBLE PRECISION AS dist
        FROM context_blocks cb
        WHERE p_semantic_mode = 'exact'        -- One-Time-Filter-Gate
          AND cb.embedding IS NOT NULL         -- Semantik-Delta 2 (§4.5): nur im exakt-Arm
          AND NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
        LIMIT p_exact_cap                      -- konstruktiver Deckel (§4.3a); bindet nach Wächter-Pass nur im Rest-Race (§5.6)
    ),
    semantic_exact AS (
        SELECT
            ep.id,
            ROW_NUMBER() OVER (ORDER BY ep.dist, ep.id) AS rank,   -- deterministischer Tiebreak
            1.0 - ep.dist AS cos_sim
        FROM exact_pool ep
        ORDER BY ep.dist, ep.id
        LIMIT 75
    ),
    semantic AS (
        SELECT sa.id, sa.rank, sa.cos_sim FROM semantic_ann sa
        UNION ALL
        SELECT se.id, se.rank, se.cos_sim FROM semantic_exact se
    ),
    fulltext_de AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query)),
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_de, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (
              cb.ts_de @@ plainto_tsquery('german', p_query)
              OR cb.ts_de @@ plainto_tsquery('german', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_de @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    fulltext_en AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced)),
                    CASE WHEN p_query_or IS NOT NULL AND p_query_or != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_query_or))
                         ELSE 0.0 END,
                    CASE WHEN p_temporal IS NOT NULL AND p_temporal != ''
                         THEN ts_rank_cd(cb.ts_en, websearch_to_tsquery('simple', p_temporal))
                         ELSE 0.0 END
                ) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (
              cb.ts_en @@ plainto_tsquery('english', p_query)
              OR cb.ts_en @@ plainto_tsquery('english', p_query_spaced)
              OR (p_query_or IS NOT NULL AND p_query_or != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_query_or))
              OR (p_temporal IS NOT NULL AND p_temporal != ''
                  AND cb.ts_en @@ websearch_to_tsquery('simple', p_temporal))
          )
        LIMIT 100
    ),
    trigram_title AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY similarity(cb.title, p_query) DESC
            ) AS rank
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND similarity(cb.title, p_query) > 0.05
        LIMIT 30
    ),
    block_mass AS (
        SELECT cb.id,
               CASE
                   WHEN array_length(cb.content_times, 1) IS NULL THEN 1.0
                   WHEN array_length(cb.content_times, 1) = 0    THEN 1.0
                   ELSE 1.0 / sqrt(array_length(cb.content_times, 1)::DOUBLE PRECISION)
               END AS mass_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
    ),
    type_factor AS (
        SELECT cb.id, COALESCE(f.factor, 1.0) AS factor
        FROM context_blocks cb
        LEFT JOIN unnest(p_damped_types, p_damped_factors) AS f(tname, factor)
               ON cb.type_name = f.tname
        WHERE NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
    ),
    rrf AS (
        SELECT
            COALESCE(s.id, d.id, e.id, g.id) AS block_id,
            (   0.45 * COALESCE(m.mass_factor, 1.0) * COALESCE(tf.factor, 1.0) * COALESCE(1.0 / (60 + s.rank), 0)
              + 0.20 * COALESCE(m.mass_factor, 1.0) * COALESCE(tf.factor, 1.0) * COALESCE(1.0 / (60 + d.rank), 0)
              + 0.25 * COALESCE(m.mass_factor, 1.0) * COALESCE(tf.factor, 1.0) * COALESCE(1.0 / (60 + e.rank), 0)
              + 0.10 * COALESCE(m.mass_factor, 1.0) * COALESCE(tf.factor, 1.0) * COALESCE(1.0 / (60 + g.rank), 0)
            )::DOUBLE PRECISION AS score,
            s.cos_sim::DOUBLE PRECISION
        FROM semantic s
        FULL OUTER JOIN fulltext_de d USING (id)
        FULL OUTER JOIN fulltext_en e USING (id)
        FULL OUTER JOIN trigram_title g USING (id)
        LEFT JOIN block_mass m ON m.id = COALESCE(s.id, d.id, e.id, g.id)
        LEFT JOIN type_factor tf ON tf.id = COALESCE(s.id, d.id, e.id, g.id)
    )
    SELECT
        r.score                    AS rrf_score,
        r.cos_sim                  AS cosine_sim,
        cb.id,
        cb.title::TEXT             AS title,
        cb.category,
        cb.tags,
        cb.content,
        cb.scope::VARCHAR(50)      AS scope,
        cb.updated_at,
        cb.type_name::TEXT         AS type_name
    FROM rrf r
    JOIN context_blocks cb ON cb.id = r.block_id
    ORDER BY r.score DESC
    LIMIT p_limit;
END;
$$;
-- @@ ctx-fold end 112_rrf_gen15_dual_arm.sql

-- @@ ctx-fold begin 113_embed_failures.sql
-- 113_embed_failures.sql
-- Achse 04 W04-2: Fehler-Memo pro Block — schließt Vorfall 2026-07-10
-- (Backfill-Endlosschleife an einem Block > Slot-Fenster) strukturell statt
-- operational. Dient dem regulären Backfill (Pfad A/B, migration_id NULL)
-- UND später der Re-Embed-Statemachine (migration_id gesetzt, W04-3+).
--
-- last_error ist NORMALISIERT, nie roher Wire-Error: Fehlerklasse + HTTP-
-- Status + header-bereinigter Auszug <=500 Zeichen. Die Konstruktion
-- passiert im Go-Worker (store.NormalizeEmbedError) — Embed-TEXT-Inhalte
-- dürfen die Zeile strukturell nicht erreichen (Backend-Response-Bodies
-- können Eingabe-Fragmente echoen; sensitivity-gebundene Blöcke bekämen
-- sonst ungeregelte Metadaten-Kopien). Lese-Oberfläche: server-admin-only
-- (W04-7, noch nicht gebaut).
CREATE TABLE IF NOT EXISTS context_embed_failures (
    block_id        UUID NOT NULL REFERENCES context_blocks(id) ON DELETE CASCADE,
    -- FK auf context_embed_migrations kommt in Migration 114 nach (ALTER ADD
    -- CONSTRAINT) — die Tabelle existiert hier noch nicht (W04-3).
    migration_id    UUID,
    attempts        INT NOT NULL DEFAULT 1,
    last_error      TEXT NOT NULL,
    last_class      TEXT NOT NULL, -- 'wire','oversize','sensitivity_ineligible','store',…
    next_attempt_at TIMESTAMPTZ NOT NULL,
    first_seen      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- PK-Ersatz (migration_id ist NULL-fähig, ein nackter PK/UNIQUE(block_id,
-- migration_id) ließe beliebig viele Backfill-Zeilen je Block zu, da NULL
-- nie mit NULL matcht): zwei Partial-Unique-Indexe, einer je Regime.
CREATE UNIQUE INDEX IF NOT EXISTS idx_embed_failures_backfill
    ON context_embed_failures (block_id) WHERE migration_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_embed_failures_migration
    ON context_embed_failures (block_id, migration_id) WHERE migration_id IS NOT NULL;

-- Peek-Träger für Pfad A/B: join-getriebene Backoff-Prüfung im Pending-
-- Prädikat ohne Memo-Tabellen-Scan.
CREATE INDEX IF NOT EXISTS idx_embed_failures_next_attempt
    ON context_embed_failures (next_attempt_at) WHERE migration_id IS NULL;
-- @@ ctx-fold end 113_embed_failures.sql

-- =============================================================================
-- Version ledger
-- =============================================================================
-- Records every folded version under the filename and checksum of the file
-- that defined it. Files from 031 onwards mostly record themselves inside
-- their own body (without a checksum — the column postdates them, migration
-- 108); this ledger fills those gaps and stamps the checksums, so a fresh
-- install ends up with the same _migrations rows an upgraded database has.
-- The UPDATE branch only ever fills a NULL checksum; it never rewrites a
-- value or a filename an older binary wrote.
INSERT INTO _migrations (version, filename, checksum) VALUES
    (1, '001_initial.sql', '86fb294a06007cf6be77e0622b4149df19b4e9802e5d844977b1192c7982125c'),
    (2, '002_scale_columns.sql', 'ea4ddfa9aad2bb17bbc7ba54d9c8d37a97b06adf2fdc84fe0a2fb2c8c984facb'),
    (3, '003_pg_functions.sql', '5174354d020a5186edee2522013cd0fc4fef73fb6a0232c1048c4511ae9cd14f'),
    (4, '004_notify_triggers.sql', '94552f413b5dbc116d72f400d0416aa25cbc1d9371e90d485077ef842393cac7'),
    (5, '005_scope_unique.sql', 'ba5aa82feb3870b93eb014bba8d7dd8ae87d1470f1ec3cf166848bbb807f095c'),
    (6, '006_temporal_rrf.sql', '7077e4aac9b07ec3ad63e36fa342072ccdea1e72e3e46e6c5ebcd5ad73554a7e'),
    (7, '007_temporal_gravity.sql', '71455ad9417f574008e4dd19a644da8c2e0ad36efdf371eaa43a8d369423c2d0'),
    (8, '008_gin_range_prefilter.sql', '9b06724d2a5f9a1a26738ee3eea0e25f6d8ee104d3ae596404472d4483ce9b1a'),
    (9, '009_temporal_dimension.sql', 'e61c7e6a8b9103d59d4ba076f02ffaafeaacd2b85419a5b55bdfb15c8d52936c'),
    (10, '010_content_dates_drop_generated.sql', '00aa71a29d94ef4ec5ba9520a4a27d317036d07303dac53b88329251ba37c361'),
    (11, '011_guard_chunk_filter.sql', '7102b466bf03640ae4e1b65911f2b7f56518ae0ac1a53a87bfee541591f19233'),
    (12, '012_ingestion_sources.sql', '5061afea81713a3fc9fad73dfc1720b62d57a5d8a5e6655ee88de815059369cf'),
    (13, '013_link_dimension.sql', '7e3665edad9af8223cb7efc7a15d167bc299d2666640644e88a32c1038df491f'),
    (14, '014_security_hardening.sql', 'ba3f06c01fc16cf343fbd7b269f945ab7449ed9e48ffa84b725453acbb5add9e'),
    (15, '015_blob_scope_unique.sql', 'cce6b75ccbf7647948d7f8791c5157763806b2a021e268712d9d1202d4e6a642'),
    (16, '016_dream.sql', '0220bfcf644d2ea75f6304a69a67c24830ed80e653bc0fbda691401c43d03078'),
    (17, '017_dream_indexes.sql', '291a09664ed6684f107cd271688c18c2f8ab1e61b5627c4551288e560ddde170'),
    (18, '018_fts_or_hybrid.sql', '95833a0e5a86c87e76df95c1d6c20a40828a159215dc5c0e55fa5f06d1cca176'),
    (19, '019_monthday_seasonal.sql', 'f5b8335f940db47613530dde76ba2d0ed86c8f79515af9f6fe4821f855690083'),
    (20, '020_content_times.sql', '1db2768ef7ed87c6ae3b0ab1d627c8e44aedf92a0844dd81ad982cfaed88c453'),
    (21, '021_created_at_anchors.sql', '38cf11371040a28458a600b2bd64b2c38e56247d6dd649f26d803bdd2aff111c'),
    (22, '022_stats_indexes.sql', '3ca01871ac506b081f160eaf19984330f7c7e27eceb59435328a1563e603d168'),
    (23, '023_oauth_clients.sql', 'cc476f091ebfe9ba255489deb8e0bd3645d1be26c1730d83bca669bafc28735c'),
    (24, '024_dream_links_raw_confidence.sql', '8d9627363f754ff5423f52ad3f9c1e2ab34636f860e7a6d43eb7bb9729617a50'),
    (25, '025_llm_log.sql', '133b6889fa46fd016b77bf4f34b3aeeaf8c7321bc17e6dc918e1e30ee2381b7e'),
    (26, '026_embed_cache.sql', 'd1844aadbbccf9ff0a806f62a7791f4ebc1f68a5224f744a00143f381e3eebe9'),
    (27, '027_block_dream_keywords.sql', '34dd8d3183af72a7429050aa642bb44b114c58b0518c9f7be64d59a97d22d1b7'),
    (28, '028_block_temporal_validated.sql', 'fca9651715d746874bcbf88757d20f423690c29be363aacaadba70008420dae7'),
    (29, '029_block_is_meta.sql', 'fb82002fa1de5fbdab65d63062b4c8fa0f5950264c6ff42814c6f8960cc21cc6'),
    (30, '030_rrf_mass_factor.sql', 'd5efb8bd2d4b043cb0a7e1236779105ca80d69f1ccf4fe105cdab9e7d3f1e387'),
    (31, '031_dream_links_recurrent.sql', 'a1b7c9ad04ad68f2e5dce898747cd5542c3100a816c1b8c00a0dd47065c180aa'),
    (32, '032_audit_blocks_is_meta.sql', '26abad055261609e57dd02ae96424ccf354489d26809ebf998a231e71af1f012'),
    (33, '033_rrf_is_meta_filter.sql', '0ffa34bfc50767d8655b7d45a6b94fc51114c6b260886dc7a5507613996fcea8'),
    (34, '034_audit_blocks_unmeta.sql', 'b115f295aae277786303e6a967d0bfccf65a7abaa22a4b315ef987510f6f0738'),
    (35, '035_block_role_classification.sql', '824f8ba54eb0c5b003e942bfbbf3c453ae5a5a45616a6671f131af4cbf6ddc78'),
    (36, '036_rrf_block_role_filter.sql', '41dde03b28bdfc24aad5cdd6ce5a14c74597edf965332bfb9a17f1a9857ce140'),
    (37, '037_rrf_role_damping_tuning.sql', '90d2941daa7b81878c9ecf42d842040ba88a436dbe9c698eee1057e949770e6f'),
    (38, '038_rrf_role_damping_revert.sql', 'e2f0305b20fa28e7a6f812ebb8480e820b8a9fce0ca0b1a4db393f9dee18d438'),
    (39, '039_rrf_query_aware_damping.sql', 'd79280fdd0217a46d2b31c9d01781805989fe7cef6a6cabfe7d5b0c06d925e1b'),
    (40, '040_audit_trail_classification_extension.sql', 'c36c4ebbe909e59d5258e7d9bdb1d6ccbfc7f39ba07741cb6023efcddb375209'),
    (41, '041_dream_links_cleanup_dangling.sql', 'cf792d5abe1cc44dfe8fc619851dfd986e4270d18de4b56a819285eb51500c9b'),
    (42, '042_dream_full_reset.sql', 'd8f834714ff2c69e394f4f3980d859a4d25e0417dd20f3412a498b5128d5f7be'),
    (43, '043_supersedes_direction_swap.sql', '093866acc63f0acb198b54172a25fd6a2cad201302e6a9cfcd9fb0aba97ea8f9'),
    (44, '044_title_text.sql', 'ee1ca87bc93e274b913dcd0a40d91680a996f6ef2bc6a76aa04a633688f773ed'),
    (45, '045_drop_chk_scope.sql', 'ac028610895587ce2d80a0dd85ce94d010fc640bdb5d4f7a374b343993b852c6'),
    (46, '046_write_log_block_title_text.sql', 'e72114b5c81dfb7fe96d486d7b7cb44df7bcc2dadac256863e5de7855be99cdc'),
    (47, '047_scope_varchar50.sql', '5189fa68845db09735ece4e2ceb728f0f583ce3594be48ac2be57e791f832a3a'),
    (48, '048_rrf_exclude_params.sql', '831a02ec38b694aa3a55c40e668b2d6613a6ad175577f8916d6fe79a834a6583'),
    (49, '049_dream_backoff.sql', '8ace0ba802128f83bc8fbffa3a35868c6e7586d8db99ecc2ff50b51bdc2e4937'),
    (50, '050_dream_links_graph_traversal.sql', 'bba524147c9c7a0cdfe34810505ed7d746c6dd3cfcf8560ab8159c53bbff6352'),
    (51, '051_settings_store.sql', 'e86db07d25c8ca577dfb65ebfe15da7fe44743679745900041c73efff385fa00'),
    (52, '052_api_key_admin.sql', '53ce058cdea48d8bb0ea12706c7947fb688a61136beb35a8318afb45e1d94884'),
    (53, '053_backend_pool.sql', '58989fd2bfcae937c389d2718f0620e9998dd7b47af445d94f6f6fd2c5cda01d'),
    (54, '054_llmlog_backend_telemetry.sql', 'bf71364f6647fe590efc27087d6116196f7183dae3011dc797cfbe283d230571'),
    (55, '055_block_sensitivity.sql', 'b9269e779f05865d7dfd002cc31559a6909ad62f1355a9eb476e9e49d5f19c9e'),
    (56, '056_chat_sessions.sql', 'a9e25c949f71505f629ad0aea71674dca52c79ec9c182a9d36f832464e835f22'),
    (57, '057_graph_overview.sql', 'd6f3977bf1596a1c3e94c6672d10ab18fb7387816f47c9ce62dc53f0319806d4'),
    (58, '058_scope_generalize.sql', '33482320b3ab4b773e3f4e2c6d843d75ea0fe60db1e8b814baed215de3c66ab7'),
    (59, '059_tenants_hybrid.sql', '31312634e6d3e4c954b07f0d689cfc3a8833308cb3f56cc96a9a48795b055071'),
    (60, '060_ctx_auth_tenant.sql', '10ed5219ae49b143aa43ac31eef4c26df3eaf41036fc0e5ab672306286f93436'),
    (61, '061_tenant_grants.sql', 'cab06b2e7ca18c93e2ea0c3e5b0d68d4312da07481b6261ebadf20c634a2032e'),
    (62, '062_backend_pool_tenant.sql', 'fd898b2d0eb89e52fe2344cf0568f4d9c259b8da9d9432a6b00b9a34288f2685'),
    (63, '063_tenant_quota.sql', 'd3f0fe7c57296063d8d1c75ad7ac89b2439bf6bd0f1ca80aa6fe8ca85d7a35f3'),
    (64, '064_settings_tenant_index.sql', '5d0fd32cb14e11fb75c8e1a2a91bae81162b1d3c18fc0d239a2a8803ff87154b'),
    (65, '065_settings_notify_scope.sql', 'ccb79a5c3f5063589e454ad9578340289a03bbdad7886ad6eb542beb01b6d387'),
    (67, '067_block_grants.sql', '0452b0ad91a35d3e63b7c28bc63ff914eab53aad14df5c0781d81f26ee157b43'),
    (68, '068_rrf_block_grants.sql', '1a2bf30fb51dd35194fae82a5462a65b842f0822dd4c9ec02862293136e1f5a3'),
    (69, '069_tenant_limits.sql', '2117acf9f6e13ca87d67319d8cb9e9182a6afc778a39367c7bfa964888011345'),
    (70, '070_lifecycle_state_rename.sql', 'e6f32686347c4d8c22c41b329cb55f97eca325312d60d7420ea5eaa4ed11bb7b'),
    (71, '071_type_name_rename.sql', '2dc9704409a3901e331910bef86d7f39a08b5a033f680314cbf7597849c845b3'),
    (72, '072_block_type_registry.sql', '9d91f65ff5cd158a634d7cf18ef72d74ca2e9d8f7d9122cfc83921c419d4cf42'),
    (73, '073_rrf_policy_params.sql', '59296a7ef60224ea9cb5d234e5d5c7a9d1d414a13021d099001c16bbcc44be04'),
    (74, '074_guard_check_type_policy.sql', '9a7f5f61abf87dd0a095885bba5ff11a766a0da374318fc7f373ec2c94a8e694'),
    (75, '075_drop_is_meta.sql', 'a79f78c6209df98f5366aff3f0416f1d5a0ba6f60ab627ef5678fb6446ffe509'),
    (76, '076_structural_links.sql', '98f8ae62a5d1b96620ecc2b3bfb040bf4406e3be93fab3d6a62626270b9ceae7'),
    (77, '077_workflow_status.sql', '57ba556f7197fa7c89a05c6ed720e6b3bd69b9db92b8b2a655bc96cfa9e472d2'),
    (78, '078_write_scopes.sql', '9451b3c73736e7b09a5047352994ba8fb5d7a963825a95fc41a26e813f1a9ef3'),
    (79, '079_context_projects.sql', 'e0ab4742551f6627b24065f3bd45b905352c17d7304a398bbae6b416d9b64b4e'),
    (80, '080_forge_sync.sql', '7a2bfb006445d4355702ac1bbcfc4caa9f4d38b8bd250eae4f1c786377bf3a42'),
    (81, '081_project_notify.sql', '98502674756a6e56c7e9aac29428d2b164c32d11d55d315a088a705f4e852af4'),
    (82, '082_webhook_inbox.sql', '0ca5706d7d0f8fddde373c91b8267843431db6f3f84eecbf88902e0d02ed7f9c'),
    (83, '083_block_type_refcount_index.sql', '6ce88e74a4c210523c3b9121d863d2e06e3359a6e30d08ec8e8fdfc857a6b95d'),
    (84, '084_issue_comment_type_seeds.sql', '0487849986b7cb6d3f5d7e4aedd5f3e239612377f5d28d125cd90fa36e2b2cfa'),
    (85, '085_comment_seed_flip.sql', '7c2b768f8f7d7ed6ddd1f8c9205ac6bb01c59fc1447302eff6ea97550fae5aee'),
    (86, '086_workflow_created_index.sql', 'd7a185748824a81c7ba88e7cbdc0bfb85019aae423e15810ba7a75c4e1234147'),
    (87, '087_member_scope.sql', '6d27892bcaca329d301d86ff4472b50497523b5070ea334264f42cd0cc7a4d76'),
    (88, '088_meta_scope_pk.sql', 'c2ec9dcad63f0c8ef43960e1babd76a7cdd7b8c3b724cbbdd97d0d55befc2701'),
    (89, '089_pending_writes.sql', 'eb11500af45ab945478dac7c21b3e6b27cff3765f5e140efd62e2f00715251b4'),
    (90, '090_confirm_writes_capability.sql', 'f394558609e32f891c7f74c7b67371ea4f1b1541fdef5b3bbd776b653ccd26c7'),
    (91, '091_dispatch_telemetry.sql', '6a3fc1272233f2cbab5c42178ad6bf4e0f96c891e2af96eca035e690571db07a'),
    (92, '092_disable_profiles.sql', 'a507ae815d90fad139488d34458187c57d10e42eadf48704c3274e2005d68cf1'),
    (93, '093_graph_category_hues.sql', 'f91872012d4e43e3ab98d110474467b848df90915fcddbdb111d5cea0c8767f9'),
    (94, '094_principal_identity.sql', '98ef4bedb21b6160c35e3fc5dee923c2acc7ed6d8830ea5428edb24c9f3837fb'),
    (95, '095_ctx_auth_principal.sql', '43e71d22a2583ef0c3906f5d5891e243e089c4d0b2b24b747a52d53089a3d50e'),
    (96, '096_audit_principal_fks.sql', '099b196e21fec7de79dccc2d3be4158c97a94d754cb0814f2498ceb90f27456d'),
    (97, '097_oauth_client_model.sql', 'ab49f18fc8c2d83be98eb369474b6bb9c0b6facbdc80ba70d1917881eeecc545'),
    (98, '098_oauth_codes.sql', 'd7bd7d37cd6653f32cd6039e6c9279f6c05816cce84f0584400e4c269a586cb1'),
    (99, '099_access_tokens.sql', '1eee6cd35fd3736c635c63320e16e955b40cded6eba8e392d104c50a63a218f5'),
    (100, '100_oauth_providers.sql', '0d713be28b82c27f255798263af9f94e30485f71c3bbb2a8cd0d52d9511a9f83'),
    (101, '101_sso_states.sql', '7ff3810686f163373e82f536eaf1feedb9737dc5cdd3128499538c2e3cfb7627'),
    (102, '102_web_sessions.sql', 'b04ab99fbfddd321311c595a3f6035e9c04ffc8b5442c90eeb99ecc3f2c614de'),
    (103, '103_audit_trail_structural_references.sql', 'de69989c6e5534406499130f1be41e03097df0df56934edfb37c977582811ea7'),
    (104, '104_struct_links_graph_traversal.sql', 'f32f21b21aa8dc031bf290eb3d38cc7490687350112604e5b203a3e0090ad41a'),
    (105, '105_structural_class_collision_preflight.sql', 'e16bd5708ac3d5644937a98f0e8c35dd5376edb2d89b19efa7d8b5074b384e74'),
    (106, '106_graph_stat_indexes.sql', '3aecea3e9002c58798c1274b70d99c00e2f87a9086b4f44b2675020cffc0135c'),
    (107, '107_checkpoint_type_seed.sql', '2807f2e17205e6153e498034ff2f25dbb36486a77a34a5b7261ec8b00c0b3b69'),
    (108, '108_migrations_checksum.sql', '29e04744438d55fb8415476264cc4775d3479e981fc4f1b25d64e580751867b4'),
    (109, '109_embed_provenance.sql', '7f7b9c4cb51bc2891a65f7ad811d991f9fad0621df6b8b8e9dd7a74c7f0d1629'),
    (110, '110_recall_runs.sql', 'e581bdab3c732507029ba27cefff0ddd1e87aa2074600025c2caaadfe7388a92'),
    (111, '111_stratify_covering_index.sql', '6656d472538dcde6f8addf8c4fa8f2bf0f92f4dfdf4f3bf2208e99cfc9b16258'),
    (112, '112_rrf_gen15_dual_arm.sql', '9037ae30a5b66bdce33ee012189459953186179861543889bc055d0e9b76a92b'),
    (113, '113_embed_failures.sql', 'ade92846b7feb681b1ae97cca30e529ded7b52e01b4d27839c0f2631df476333')
ON CONFLICT (version) DO UPDATE
    SET checksum = EXCLUDED.checksum
    WHERE _migrations.checksum IS NULL;
