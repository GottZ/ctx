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
