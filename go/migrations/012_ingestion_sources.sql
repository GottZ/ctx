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
