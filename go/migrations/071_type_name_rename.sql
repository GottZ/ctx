-- =============================================================================
-- 071_type_name_rename.sql — rename block_role → type_name + type_source
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Workflow-engine axis 01, wave T2 (design/01-type-registry.md §3.4, decision
-- D1). Sibling of 070 (block_type → lifecycle_state): this migration renames
-- the OPEN policy axis to its registry-era name:
--
--   block_role → type_name: WHAT the block is (knowledge, audit-trail,
--   reference, system-meta; future registry types: issue, comment, …). The
--   value set stays governed by the M035 CHECK constraint (its historical
--   name context_blocks_block_role_check survives the rename and only falls
--   in 073, together with the Go-side registry validation — never a deploy
--   state where neither DB nor Go validates the type names).
--
-- Steps:
--   1. RENAME COLUMN — metadata-only op, instant; the M035 partial index
--      (WHERE block_role != 'knowledge') and CHECK constraint track
--      attribute numbers and survive the rename (predicate text is
--      rewritten by PostgreSQL, constraint/index NAMES keep 'block_role').
--   2. type_source TEXT NOT NULL DEFAULT 'auto' — provenance column,
--      'auto' | 'manual', exact sensitivity_source pattern (055): a
--      'manual' value permanently overrides the auto-classifier (consumer
--      wiring is wave T4/T10; this wave only lays the column).
--   3. ctx_rrf DROP+CREATE — PL/pgSQL bodies resolve column names at
--      runtime, so without a re-create the first retrieval query after the
--      rename would fail with 42703. Body is the 068 version with ONLY the
--      column references adapted (cb.block_role → cb.type_name); signature,
--      parameter names (incl. p_block_roles_exclude — the wire field
--      block_roles_exclude stays compatible until T5/T10), CTE names and
--      semantics are byte-identical. Policy parametrisation is wave T5's
--      job (M073), not this one's.
--
-- Historical migrations (032, 035, 036, …) keep referencing block_role: on a
-- fresh DB they run at their own position, BEFORE this rename — forward-only
-- ordering guarantees validity.
--
-- Idempotent (rename guarded by column-existence check, ADD COLUMN IF NOT
-- EXISTS, DROP+CREATE, _migrations ON CONFLICT). Forward-only,
-- self-registering.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- 1. Rename, guarded for idempotency: only if the old column still exists.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'context_blocks' AND column_name = 'block_role'
    ) THEN
        ALTER TABLE context_blocks RENAME COLUMN block_role TO type_name;
    END IF;
END $$;

-- 2. Provenance column, sensitivity_source pattern (055): 'manual' wins
--    against the auto-classifier from T4 on. Backfill-free — every existing
--    row was classified by the Welle-44 hook or a migration, i.e. 'auto'.
ALTER TABLE context_blocks
    ADD COLUMN IF NOT EXISTS type_source TEXT NOT NULL DEFAULT 'auto'
        CHECK (type_source IN ('auto','manual'));

-- 3. Re-create ctx_rrf with the new column name. 068 body, column references
--    adapted only — signature and semantics identical (system-meta
--    hard-exclude, audit-trail damping via p_audit_trail_factor, grant
--    OR-arm parenthesisation untouched). Single DROP suffices: the new
--    signature equals the old one, so re-runs stay idempotent and exactly
--    ONE overload exists afterwards.
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
          AND cb.type_name != 'system-meta'
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.type_name IS NULL OR cb.type_name != ALL(p_block_roles_exclude))
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
          AND cb.type_name != 'system-meta'
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.type_name IS NULL OR cb.type_name != ALL(p_block_roles_exclude))
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
          AND cb.type_name != 'system-meta'
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.type_name IS NULL OR cb.type_name != ALL(p_block_roles_exclude))
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
          AND cb.type_name != 'system-meta'
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.type_name IS NULL OR cb.type_name != ALL(p_block_roles_exclude))
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
          AND cb.type_name != 'system-meta'
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.type_name IS NULL OR cb.type_name != ALL(p_block_roles_exclude))
    ),
    block_role_factor AS (
        SELECT cb.id,
               CASE
                   WHEN cb.type_name = 'audit-trail' THEN p_audit_trail_factor
                   ELSE 1.0
               END AS role_factor
        FROM context_blocks cb
        WHERE NOT cb.is_archived
          AND cb.type_name != 'system-meta'
          AND ( cb.scope = ANY(p_scopes)
                OR (p_granted_block_ids IS NOT NULL AND cb.id = ANY(p_granted_block_ids)) )
          AND (p_category IS NULL OR cb.category = p_category)
          AND (p_tags IS NULL OR cb.tags && p_tags)
          AND (p_categories_exclude IS NULL OR cb.category != ALL(p_categories_exclude))
          AND (p_block_roles_exclude IS NULL OR cb.type_name IS NULL OR cb.type_name != ALL(p_block_roles_exclude))
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
  VALUES (71, '071_type_name_rename.sql', now())
  ON CONFLICT (version) DO NOTHING;
