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
