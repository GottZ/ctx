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
