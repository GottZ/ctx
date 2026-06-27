-- =============================================================================
-- 068_rrf_block_grants.sql — ctx_rrf block-level read grant OR-arm (MT, Achse 07)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Multi-Tenant wave T40b (Achse 07-W5, the expensive RRF-retrieval OR-arm).
-- Builds on 067 (context_block_grants) + T40a (the cheap abruf/ID paths +
-- VisibilityPredicate grant arm). Exact spec: design/07-block-level-sharing.md
-- §2.2/§2.3/§4.2/§5.3.2 + Welle T40b.
--
-- T40a wired the row-level grant OR-arm into every CHEAP read path (GetBlock,
-- ResolveBlockID, SearchBlocks, EgoGraph, the 3 MCP handlers) via the shared
-- store.VisibilityPredicate. ctx_rrf does NOT reference that Go fragment — it
-- carries the same visibility triple SIX times inline (one WHERE per RRF
-- channel: semantic / fulltext_de / fulltext_en / trigram_title / block_mass /
-- block_role_factor). This migration is the SECOND materialisation of the OR-arm
-- (the SQL side), so a granted block surfaces in semantic retrieval too — not
-- only via direct abruf.
--
-- Mechanism: a 13th parameter p_granted_block_ids UUID[] DEFAULT NULL (M048
-- DROP+CREATE schablone, 048:31-32/45-46), and at each of the SIX CTE WHERE
-- clauses the flat term `AND cb.scope = ANY(p_scopes)` becomes an EXPLICITLY
-- PARENTHESISED disjunction:
--   AND ( cb.scope = ANY(p_scopes)
--         OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
--
-- CRITICAL (design/07 §4.2/§5.3.2, operator precedence): ctx_rrf carries NO
-- parentheses around the scope term today (a flat AND chain, 048:69-77) — the
-- inner (scope OR id) parens must be CREATED. SQL binds AND tighter than OR.
-- WITHOUT the inner parens the OR binds at top level and the preceding
-- `NOT is_archived` / `block_role <> 'system-meta'` conjuncts are bypassed for
-- the grant arm → a granted ARCHIVED or system-meta block would leak. The
-- archived/system-meta conjuncts therefore stand strictly BEFORE the parens.
-- The `p_granted_block_ids IS NOT NULL` guard makes the OR a deterministic no-op
-- for every legacy caller (NULL = no block-level), exactly like the
-- `p_categories_exclude IS NULL` branch — so existing callers are byte-identical.
--
-- Return-Type UNCHANGED → backward-compatible additive. Function-only, no
-- table/column change: test.sh table count unchanged (cf. 060/065). Idempotent
-- (DROP IF EXISTS + CREATE OR REPLACE + ON CONFLICT DO NOTHING), forward-only,
-- self-registering (M031+ convention). Out-of-order gap (066 absent) tolerated
-- by the runner's per-version EXISTS check.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- Drop the current 12-param signature (048) and the new 13-param signature (for
-- idempotent re-runs) — M048 pattern (048:31-32). CREATE OR REPLACE alone would
-- leave the old 12-arg overload alive alongside the new one.
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT, DOUBLE PRECISION, TEXT[], TEXT[]);
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT, DOUBLE PRECISION, TEXT[], TEXT[], UUID[]);

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
    p_block_roles_exclude   TEXT[] DEFAULT NULL,
    p_granted_block_ids     UUID[] DEFAULT NULL
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
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
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
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
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
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
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
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
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
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
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
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
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
  VALUES (68, '068_rrf_block_grants.sql', now())
  ON CONFLICT (version) DO NOTHING;
