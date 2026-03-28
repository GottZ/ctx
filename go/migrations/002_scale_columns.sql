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
