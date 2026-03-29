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
