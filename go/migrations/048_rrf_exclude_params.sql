-- =============================================================================
-- 048_rrf_exclude_params.sql — ctx_rrf categories_exclude + block_roles_exclude
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle 47 / v2.0.0 C2: API-side exclude parameters für /api/query.
--
-- Trigger: CRAG-Bench n=10 movie (Session 38c) zeigte topic-map-private Block
-- (category='index', block_role='audit-trail'/'synthesis') als Slot-Stealer
-- in 4/10 Variant-A Top-5. In n=10 kein Score-Impact, aber strukturelles
-- Risk-Faktor bei größerem Korpus.
--
-- Lösung: zwei optionale Parameter ans Ende der ctx_rrf-Signatur:
--   - p_categories_exclude TEXT[]   — Blöcke mit category IN(...) ausschließen
--   - p_block_roles_exclude TEXT[]  — Blöcke mit block_role IN(...) ausschließen
--
-- Schema-Defaults NULL = no-op exclude (backward-kompatibel). Existing caller
-- (dream-cycle, Tests) brauchen keine Code-Änderung.
--
-- WHERE-Klausel-Pattern (siehe semantic-CTE, identisch in fulltext_de/en/
-- trigram_title/block_mass/block_role_factor):
--   AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
--   AND (p_block_roles_exclude IS NULL OR cb.block_role IS NULL
--        OR cb.block_role != ALL(p_block_roles_exclude))
--
-- block_role IS NULL-Branch verhindert NULL != ALL(...)-Pitfall: in PG ist
-- NULL != ALL(ARRAY[...]) immer NULL (nicht TRUE), filtert sonst Legacy-
-- Blocks ohne expliziten block_role aus. Explicit OR cb.block_role IS NULL
-- erhält sie.
-- =============================================================================

DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT, DOUBLE PRECISION);
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT, DOUBLE PRECISION, TEXT[], TEXT[]);

CREATE OR REPLACE FUNCTION ctx_rrf(
    p_embedding             halfvec(1024),
    p_query                 TEXT,
    p_query_spaced          TEXT,
    p_scopes                TEXT[],
    p_category              TEXT DEFAULT NULL,
    p_tags                  TEXT[] DEFAULT NULL,
    p_limit                 INT DEFAULT 5,
    p_temporal              TEXT DEFAULT NULL,
    p_query_or              TEXT DEFAULT NULL,
    p_audit_trail_factor    DOUBLE PRECISION DEFAULT 1.0,
    p_categories_exclude    TEXT[] DEFAULT NULL,
    p_block_roles_exclude   TEXT[] DEFAULT NULL
) RETURNS TABLE (
    rrf_score   DOUBLE PRECISION,
    cosine_sim  DOUBLE PRECISION,
    id          UUID,
    title       TEXT,
    category    VARCHAR(100),
    tags        TEXT[],
    content     TEXT,
    scope       VARCHAR(50),
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
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.block_role IS NULL OR cb.block_role != ALL(p_block_roles_exclude))
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
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.block_role IS NULL OR cb.block_role != ALL(p_block_roles_exclude))
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
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.block_role IS NULL OR cb.block_role != ALL(p_block_roles_exclude))
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
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.block_role IS NULL OR cb.block_role != ALL(p_block_roles_exclude))
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
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.block_role IS NULL OR cb.block_role != ALL(p_block_roles_exclude))
    ),
    block_role_factor AS (
        SELECT cb.id,
               CASE
                   WHEN cb.block_role = 'audit-trail' THEN p_audit_trail_factor
                   ELSE 1.0
               END AS role_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.block_role != 'system-meta'
          AND cb.scope = ANY(p_scopes)
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.block_role IS NULL OR cb.block_role != ALL(p_block_roles_exclude))
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
        r.score                    AS rrf_score,
        r.cos_sim                  AS cosine_sim,
        cb.id,
        cb.title::TEXT             AS title,
        cb.category,
        cb.tags,
        cb.content,
        cb.scope::VARCHAR(50)      AS scope,
        cb.updated_at
    FROM rrf r
    JOIN context_blocks cb ON cb.id = r.block_id
    ORDER BY r.score DESC
    LIMIT p_limit;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (48, '048_rrf_exclude_params.sql', now())
  ON CONFLICT (version) DO NOTHING;
