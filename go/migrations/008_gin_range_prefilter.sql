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
