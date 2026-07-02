-- =============================================================================
-- 073_rrf_policy_params.sql — ctx_rrf policy-parametrisiert + CHECK-Drop
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Workflow-engine axis 01, wave T5 (design/01-type-registry.md §3.5). The
-- 14th ctx_rrf generation: type visibility and damping become PARAMETERS fed
-- from the block-type registry (M072) — the last hardcoded type literals in
-- the retrieval path fall.
--
-- THE semantic switch of the axis (fail-open → fail-closed, §5.1a):
--
--   068/071:  cb.type_name != 'system-meta'          -- EXCLUDE literal.
--             An unregistered ("rogue") type name is NOT in the exclude
--             list ⇒ full-pass. Fail-OPEN.
--   073:      cb.type_name = ANY(p_types_visible)    -- ALLOWLIST parameter.
--             Only registered, retrieval-visible types pass. A rogue
--             type name (SQL INSERT past Go) is invisible until a
--             registry row carries it. NULL/empty allowlist ⇒ 0 rows —
--             deliberately hard; Go guarantees never-NULL analogous to
--             the len(scopes)==0 reject (rrf/search.go).
--
-- Damping generalised (replaces block_role_factor + p_audit_trail_factor):
-- parallel arrays p_damped_types/p_damped_factors (unnest join). The intent
-- lift moved to Go (blocktype.Set.DampedTypesFor): a lifted type is simply
-- absent from the arrays ⇒ COALESCE factor 1.0. p_audit_trail_factor is
-- gone WITHOUT replacement (its semantics live in the arrays).
--
-- Parenthesisation invariant (068 header, §3.5 invariant 2): the archived +
-- BOTH type conjuncts stand strictly BEFORE the (scope OR grant) parens — a
-- granted block with an excluded/rogue type stays invisible (§5.3 probe).
--
-- RETURNS gains type_name (UI badges / aggregate-to-parent fold need the
-- type on every result row — seam §2.8; wire exposure is T10's job).
--
-- CHECK-Drop: context_blocks_block_role_check (M035 enum, name survived the
-- 071 column rename) falls — the registry is the authority on type names
-- now (Go write validation T10 + allowlist read path here). A rogue INSERT
-- past Go becomes POSSIBLE and is WANTED that way: fail-closed lives in the
-- read path, observability in the orphan sweep (§5.1a), not in a hard enum
-- that would block every new workflow type.
--
-- Wire compat: the REST field block_roles_exclude stays (seam 17; rename
-- alias types_exclude is T10) — Go maps it onto p_types_exclude.
--
-- Function-only + constraint drop, no table/column change: test.sh T07
-- table/column counts unchanged. Idempotent (DROP IF EXISTS both
-- signatures + CREATE OR REPLACE + ON CONFLICT DO NOTHING), forward-only,
-- self-registering (M031+ convention). On a fresh DB 048/068/071 run at
-- their positions first; this file replaces the function afterwards.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- 1. The M035 enum CHECK falls: the registry is the type-name authority.
ALTER TABLE context_blocks DROP CONSTRAINT IF EXISTS context_blocks_block_role_check;

-- 2. Drop the 071 signature (and the new one for idempotent re-runs) —
--    M048/068 pattern: CREATE OR REPLACE alone would leave the old overload
--    alive alongside the new one (42725 ambiguity for legacy callers).
--    ZOMBIE SWEEP (T5 finding): the 003 (7-param) and 006 (8-param)
--    generations were never dropped by ANY later migration — 020 removed
--    the DATE variants, 030-039 the 9-param one, but these two survived the
--    whole chain (fresh DB: `\df ctx_rrf` = 3 signatures pre-073). They are
--    dead weight and an overload-resolution hazard for short positional
--    calls; the T5 single-signature gate flushes them here.
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT);
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT);
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT, DOUBLE PRECISION, TEXT[], TEXT[], UUID[]);
DROP FUNCTION IF EXISTS ctx_rrf(halfvec, TEXT, TEXT, TEXT[], TEXT, TEXT[], INT, TEXT, TEXT, TEXT[], TEXT[], DOUBLE PRECISION[], TEXT[], TEXT[], UUID[]);

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
    p_types_visible         TEXT[] DEFAULT NULL,   -- ALLOWLIST (fail-closed): NULL/leer ⇒ 0 Treffer
    p_damped_types          TEXT[] DEFAULT NULL,   -- parallel zu p_damped_factors
    p_damped_factors        DOUBLE PRECISION[] DEFAULT NULL,
    p_categories_exclude    TEXT[] DEFAULT NULL,
    p_types_exclude         TEXT[] DEFAULT NULL,   -- Request-Level-Exclude (vorm. p_block_roles_exclude)
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
    updated_at  TIMESTAMPTZ,
    type_name   TEXT
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
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
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
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
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
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
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
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
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
          AND cb.type_name = ANY(p_types_visible)
          AND (p_types_exclude IS NULL OR cb.type_name != ALL(p_types_exclude))
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
    ),
    type_factor AS (
        SELECT cb.id, COALESCE(f.factor, 1.0) AS factor
        FROM context_blocks cb
        LEFT JOIN unnest(p_damped_types, p_damped_factors) AS f(tname, factor)
               ON cb.type_name = f.tname
        WHERE NOT cb.is_archived
          AND cb.type_name = ANY(p_types_visible)
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
    ),
    rrf AS (
        SELECT
            COALESCE(s.id, d.id, e.id, g.id) AS block_id,
            (   0.45 * COALESCE(m.mass_factor, 1.0) * COALESCE(tf.factor, 1.0) * COALESCE(1.0 / (60 + s.rank), 0)
              + 0.20 * COALESCE(m.mass_factor, 1.0) * COALESCE(tf.factor, 1.0) * COALESCE(1.0 / (60 + d.rank), 0)
              + 0.25 * COALESCE(m.mass_factor, 1.0) * COALESCE(tf.factor, 1.0) * COALESCE(1.0 / (60 + e.rank), 0)
              + 0.10 * COALESCE(m.mass_factor, 1.0) * COALESCE(tf.factor, 1.0) * COALESCE(1.0 / (60 + g.rank), 0)
            )::DOUBLE PRECISION AS score,
            s.cos_sim::DOUBLE PRECISION
        FROM semantic s
        FULL OUTER JOIN fulltext_de d USING (id)
        FULL OUTER JOIN fulltext_en e USING (id)
        FULL OUTER JOIN trigram_title g USING (id)
        LEFT JOIN block_mass m ON m.id = COALESCE(s.id, d.id, e.id, g.id)
        LEFT JOIN type_factor tf ON tf.id = COALESCE(s.id, d.id, e.id, g.id)
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
        cb.updated_at,
        cb.type_name::TEXT         AS type_name
    FROM rrf r
    JOIN context_blocks cb ON cb.id = r.block_id
    ORDER BY r.score DESC
    LIMIT p_limit;
END;
$$;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (73, '073_rrf_policy_params.sql', now())
  ON CONFLICT (version) DO NOTHING;
