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
