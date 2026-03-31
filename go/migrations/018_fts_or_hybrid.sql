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
