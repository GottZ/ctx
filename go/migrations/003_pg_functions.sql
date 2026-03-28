-- =============================================================================
-- 003_pg_functions.sql — PG Functions for ctx business logic
-- =============================================================================
-- Three functions that encapsulate core data logic:
--   1. ctx_auth(api_key)       — authenticate API key, return scopes
--   2. ctx_rrf(...)            — 4-way weighted Reciprocal Rank Fusion search
--   3. ctx_guard_check(block_id) — duplicate detection for a single block
--
-- Prerequisites: pgcrypto, pg_trgm, vector extensions
-- Target DB: context_store (user: context_user)
-- =============================================================================

-- -----------------------------------------------------------------------------
-- 1. ctx_auth — Authenticate API key and return scope information
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION ctx_auth(p_api_key TEXT)
RETURNS TABLE (
    api_key_id UUID,
    home_scope VARCHAR(20),
    allowed_scopes TEXT[],
    read_scopes TEXT[],
    is_valid BOOLEAN
) LANGUAGE plpgsql AS $$
DECLARE
    v_key_hash TEXT;
    v_api_key_id UUID;
    v_home_scope VARCHAR(20);
    v_allowed_scopes TEXT[];
BEGIN
    -- Compute SHA-256 hash of the provided API key
    v_key_hash := encode(digest(p_api_key, 'sha256'), 'hex');

    -- Authenticate: update last_used_at and return key info
    UPDATE context_api_keys
    SET last_used_at = now()
    WHERE context_api_keys.key_hash = v_key_hash
      AND context_api_keys.active = true
    RETURNING
        context_api_keys.id,
        context_api_keys.home_scope,
        context_api_keys.allowed_scopes
    INTO v_api_key_id, v_home_scope, v_allowed_scopes;

    -- Check if we found a valid key
    IF v_api_key_id IS NULL THEN
        -- Invalid key: return sentinel values
        api_key_id     := NULL;
        home_scope     := '__UNAUTHORIZED__';
        allowed_scopes := '{}'::TEXT[];
        read_scopes    := '{}'::TEXT[];
        is_valid       := false;
        RETURN NEXT;
        RETURN;
    END IF;

    -- Valid key: build read_scopes = [home_scope] || allowed_scopes
    api_key_id     := v_api_key_id;
    home_scope     := v_home_scope;
    allowed_scopes := v_allowed_scopes;
    read_scopes    := ARRAY[v_home_scope::TEXT] || COALESCE(v_allowed_scopes, '{}'::TEXT[]);
    is_valid       := true;
    RETURN NEXT;
    RETURN;
END;
$$;


-- -----------------------------------------------------------------------------
-- 2. ctx_rrf — 4-Way Weighted Reciprocal Rank Fusion
-- -----------------------------------------------------------------------------
-- Weights: Semantic 0.45, EN-FTS 0.25, DE-FTS 0.20, Trigram 0.10 (k=60)
-- Semantic LIMIT 75, Trigram LIMIT 30 (min similarity 0.05)
-- All CTEs scope-filtered, optional category + tags
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION ctx_rrf(
    p_embedding    halfvec(1024),
    p_query        TEXT,
    p_query_spaced TEXT,
    p_scopes       TEXT[],
    p_category     TEXT DEFAULT NULL,
    p_tags         TEXT[] DEFAULT NULL,
    p_limit        INT DEFAULT 5
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
    -- Enable relaxed HNSW iterative scan for this transaction
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
                    ts_rank_cd(cb.ts_de, plainto_tsquery('german', p_query_spaced))
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
          )
        LIMIT 100
    ),
    fulltext_en AS (
        SELECT
            cb.id,
            ROW_NUMBER() OVER (
                ORDER BY GREATEST(
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query)),
                    ts_rank_cd(cb.ts_en, plainto_tsquery('english', p_query_spaced))
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
            (0.45 * COALESCE(1.0 / (60 + s.rank), 0)
          +  0.20 * COALESCE(1.0 / (60 + d.rank), 0)
          +  0.25 * COALESCE(1.0 / (60 + e.rank), 0)
          +  0.10 * COALESCE(1.0 / (60 + g.rank), 0))::DOUBLE PRECISION AS score,
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


-- -----------------------------------------------------------------------------
-- 3. ctx_guard_check — Check a single block for near-duplicates
-- -----------------------------------------------------------------------------
-- Thresholds: >= 0.98 near_duplicate, >= 0.92 needs_review, < 0.92 clean
-- Uses HNSW Top-1 nearest neighbor (excl. self, excl. archived)
-- Reports cross-scope status
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION ctx_guard_check(p_block_id UUID)
RETURNS TABLE (
    decision        VARCHAR,
    top_similarity  NUMERIC,
    matched_id      UUID,
    matched_title   VARCHAR,
    matched_scope   VARCHAR,
    is_cross_scope  BOOLEAN
) LANGUAGE plpgsql AS $$
DECLARE
    v_embedding     vector(1024);
    v_scope         VARCHAR(20);
    v_matched_id    UUID;
    v_matched_title VARCHAR(255);
    v_matched_scope VARCHAR(20);
    v_similarity    NUMERIC;
BEGIN
    -- Load the block's embedding and scope
    SELECT cb.embedding, cb.scope
    INTO v_embedding, v_scope
    FROM context_blocks cb
    WHERE cb.id = p_block_id;

    -- If block not found or has no embedding, return clean with no match
    IF v_embedding IS NULL THEN
        decision       := 'clean';
        top_similarity := 0;
        matched_id     := NULL;
        matched_title  := NULL;
        matched_scope  := NULL;
        is_cross_scope := false;
        RETURN NEXT;
        RETURN;
    END IF;

    -- Find Top-1 nearest neighbor (excluding self, excluding archived)
    SELECT
        cb.id,
        cb.title,
        cb.scope,
        round(
            (1 - (cb.embedding::halfvec(1024) <=> v_embedding::halfvec(1024)))::numeric,
            4
        )
    INTO v_matched_id, v_matched_title, v_matched_scope, v_similarity
    FROM context_blocks cb
    WHERE cb.id != p_block_id
      AND NOT cb.is_archived
      AND cb.embedding IS NOT NULL
    ORDER BY cb.embedding::halfvec(1024) <=> v_embedding::halfvec(1024)
    LIMIT 1;

    -- No neighbors found
    IF v_matched_id IS NULL THEN
        decision       := 'clean';
        top_similarity := 0;
        matched_id     := NULL;
        matched_title  := NULL;
        matched_scope  := NULL;
        is_cross_scope := false;
        RETURN NEXT;
        RETURN;
    END IF;

    -- Apply thresholds
    IF v_similarity >= 0.98 THEN
        decision := 'near_duplicate';
    ELSIF v_similarity >= 0.92 THEN
        decision := 'needs_review';
    ELSE
        decision := 'clean';
    END IF;

    -- Determine cross-scope status
    -- Cross-scope = match is NOT in same scope AND match is NOT shared
    top_similarity := v_similarity;
    matched_id     := v_matched_id;
    matched_title  := v_matched_title;
    matched_scope  := v_matched_scope;
    is_cross_scope := (v_matched_scope != v_scope AND v_matched_scope != 'shared');

    RETURN NEXT;
    RETURN;
END;
$$;
