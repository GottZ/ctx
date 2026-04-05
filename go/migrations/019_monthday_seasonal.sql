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
