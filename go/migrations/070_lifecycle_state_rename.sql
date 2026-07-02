-- =============================================================================
-- 070_lifecycle_state_rename.sql — rename block_type → lifecycle_state
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Workflow-engine axis 01, wave T1 (design/01-type-registry.md §3.4, decision
-- D1). context_blocks carries two orthogonal type axes; this migration makes
-- the pipeline axis explicit by name:
--
--   block_type → lifecycle_state: WHERE the block stands in the pipeline
--   lifecycle (knowledge → canonical → snapshot; chunk; synthesis). The
--   column is written exclusively by mechanism code (dream promotion,
--   supersedes/revert, ingest chunks, daily report) — a closed, code-owned
--   state machine. The sibling rename block_role → type_name (the open,
--   registry-driven policy axis) follows in 071.
--
-- Steps:
--   1. RENAME COLUMN — metadata-only op, instant; indexes and constraints
--      (incl. the uq_source_chunk partial-index predicate from 012) track
--      attribute numbers and survive the rename unchanged.
--   2. NULL backfill → 'knowledge'. NULLs were produced by the historical
--      supersedes-revert (SET block_type = NULL); the Go side writes
--      'knowledge' from this wave on. The dead value 'source' (0 live rows)
--      is retired from all Go filters in the same wave — no data touch
--      needed for it.
--   3. SET NOT NULL — the column already has DEFAULT 'knowledge'; after the
--      backfill the constraint holds for all rows. Full-table validation
--      scan, trivially fast at current size and still a single short
--      ACCESS EXCLUSIVE hold at 1M+ rows (no rewrite).
--   4. ctx_guard_check DROP+CREATE — PL/pgSQL bodies resolve column names at
--      runtime, so without a re-create the first guard run after the rename
--      would fail with 42703. Body is the 011 version with ONLY the column
--      name adapted; thresholds/semantics unchanged (policy parametrisation
--      is wave T7's job, not this one's).
--
-- Historical migrations (011, 012, 042, …) keep referencing block_type: on a
-- fresh DB they run at their own position, BEFORE this rename — forward-only
-- ordering guarantees validity.
--
-- Idempotent (rename guarded by column-existence check, backfill convergent,
-- SET NOT NULL re-applicable, DROP+CREATE, _migrations ON CONFLICT).
-- Forward-only, self-registering.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- 1. Rename, guarded for idempotency: only if the old column still exists.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'context_blocks' AND column_name = 'block_type'
    ) THEN
        ALTER TABLE context_blocks RENAME COLUMN block_type TO lifecycle_state;
    END IF;
END $$;

-- 2. NULL backfill (historical supersedes-revert artefacts; 8 rows live at
--    migration authoring time).
UPDATE context_blocks SET lifecycle_state = 'knowledge' WHERE lifecycle_state IS NULL;

-- 3. Lock the invariant in: the lifecycle state machine has no NULL state.
ALTER TABLE context_blocks ALTER COLUMN lifecycle_state SET NOT NULL;

-- 4. Re-create ctx_guard_check with the new column name. 011 body,
--    name-adapted only — semantics identical (chunk exclusion, 0.98/0.92
--    thresholds, cross-scope logic untouched).
DROP FUNCTION IF EXISTS ctx_guard_check(UUID);

CREATE FUNCTION ctx_guard_check(p_block_id UUID)
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
      AND (cb.lifecycle_state IS NULL OR cb.lifecycle_state NOT IN ('chunk'))
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

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (70, '070_lifecycle_state_rename.sql', now())
  ON CONFLICT (version) DO NOTHING;
