-- =============================================================================
-- 037_rrf_role_damping_tuning.sql — audit-trail Damping 0.3 → 0.5 (Welle 40 it.4)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- M036 (Welle 40 it.2/3) hat Damping-Faktor 0.3 für audit-trail eingeführt.
-- Iteration-3-Bench-Verdict: eval-cyclic mean_pass 0.8728 vs Re-Baseline 0.9428
-- = -7pp REGRESSION durch 5 NEG flips, alle bei explicit-audit-trail-target
-- queries:
--   L-002 "session 27 testcontainer behaviour"     → 019df858 audit-trail
--   L-009 "session 22 audit ergebnis"              → 019d6f2c audit-trail
--   M-003 "dream v3 performance letzte woche"      → 019defa4 audit-trail
--   M-004 "session 24 dream vor 2 wochen"          → 019dc04b audit-trail
--   M-015 "ctx security audit anfang april 2026"   → 019d74a6 audit-trail
--
-- Bei 0.3-damping fallen audit-trail-blocks auch bei explicit-target queries
-- aus top-5. eval.sh T03 ("recent embedding changes") wurde gefixt aber zu
-- hoher Preis (5 explicit-queries verloren).
--
-- Iteration 4 (Welle 40 Hypothese): Damping 0.3 → 0.5. Damit:
--   - explicit-target queries finden audit-trail-blocks weiterhin (RRF-rank
--     dieser blocks ist bei explicit-query deutlich höher als knowledge-
--     Konkurrenz, *0.5 reicht um top-5 zu halten)
--   - generic-recent queries: knowledge-blocks haben oft vergleichbare RRF-
--     rank zu audit-trail. *0.5 dämpft audit-trail unter knowledge wenn
--     ranks ≈ gleich. T03 sollte bestehen.
--
-- Symmetrisch + idempotent: Function CREATE OR REPLACE.
-- =============================================================================

DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT);

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
          AND cb.block_role != 'system-meta'
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
          AND cb.block_role != 'system-meta'
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
          AND cb.block_role != 'system-meta'
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
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND similarity(cb.title, p_query) > 0.05
        LIMIT 30
    ),
    block_mass AS (
        SELECT cb.id,
               CASE
                   WHEN array_length(cb.content_times, 1) IS NULL THEN 1.0
                   WHEN array_length(cb.content_times, 1) = 0    THEN 1.0
                   ELSE 1.0 / sqrt(array_length(cb.content_times, 1)::DOUBLE PRECISION)
               END AS mass_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
    ),
    -- Welle 40 (M037): per-block role_factor. audit-trail wird *0.5 gedämpft
    -- (M036 hatte 0.3, war zu aggressiv — fünf explicit-target queries
    -- verloren ihre audit-trail-targets aus top-5). 0.5 erlaubt explicit-
    -- target Findung weiter und dämpft trotzdem in generic-recent queries.
    block_role_factor AS (
        SELECT cb.id,
               CASE
                   WHEN cb.block_role = 'audit-trail' THEN 0.5
                   ELSE 1.0
               END AS role_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
    ),
    rrf AS (
        SELECT
            COALESCE(s.id, d.id, e.id, g.id) AS block_id,
            (   0.45 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + s.rank), 0)
              + 0.20 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + d.rank), 0)
              + 0.25 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + e.rank), 0)
              + 0.10 * COALESCE(m.mass_factor, 1.0) * COALESCE(rf.role_factor, 1.0) * COALESCE(1.0 / (60 + g.rank), 0)
            )::DOUBLE PRECISION AS score,
            s.cos_sim::DOUBLE PRECISION
        FROM semantic s
        FULL OUTER JOIN fulltext_de d USING (id)
        FULL OUTER JOIN fulltext_en e USING (id)
        FULL OUTER JOIN trigram_title g USING (id)
        LEFT JOIN block_mass m ON m.id = COALESCE(s.id, d.id, e.id, g.id)
        LEFT JOIN block_role_factor rf ON rf.id = COALESCE(s.id, d.id, e.id, g.id)
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

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (37, '037_rrf_role_damping_tuning.sql', now())
  ON CONFLICT (version) DO NOTHING;
