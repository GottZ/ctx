-- =============================================================================
-- 011_guard_chunk_filter.sql — Exclude chunks from Guard nearest-neighbor search
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- BUG-1: ctx_guard_check() matched source blocks against their own chunks
-- (similarity >> 0.98), triggering auto-archive of the source. Fix: exclude
-- block_type='chunk' from the nearest-neighbor candidate set.
-- =============================================================================

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

    -- Find Top-1 nearest neighbor (excluding self, excluding archived, excluding chunks)
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
      AND (cb.block_type IS NULL OR cb.block_type NOT IN ('chunk'))
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
