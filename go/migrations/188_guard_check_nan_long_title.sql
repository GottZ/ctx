-- =============================================================================
-- 188_guard_check_nan_long_title.sql — ctx_guard_check answers a zero-norm
-- vector clean and a neighbour title of any length (issue #396)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Two defects of ctx_guard_check (generation of 173), both older than the
-- exact check of a small candidate circle (guard.guardCheckSourceExact,
-- issue #381), which answered them differently on purpose:
--
-- 1. A vector of zero norm in halfvec(1024) (all zeros, or components that
--    round to zero in half precision) has no cosine distance: pgvector
--    answers NaN, round() keeps it as numeric NaN, and NaN >= threshold is
--    true. The HNSW index holds no such vector, so through the index the
--    function never matched one as a candidate; for a zero-norm CHECKED
--    block it compared NaN against the first candidate the index returned
--    and answered near_duplicate, which archives the checked block (flags
--    it, for a type in flag mode). Without an HNSW index to serve the query
--    (a dropped or invalid index, a REINDEX), the plan the guard batch's
--    switches disable sorted every candidate by distance, NaN after every
--    number: a block whose only candidates have zero norm was answered
--    near_duplicate as well.
-- 2. The matched title went through a VARCHAR(255) variable. context_blocks.
--    title is text (113_baseline), the store API takes titles up to 500
--    bytes (handler/context_store.go): a nearest neighbour whose title is
--    longer than 255 characters failed the check with 22001 (a title whose
--    excess characters are all blanks was cut instead), and the checked
--    block stayed pending for good — the batch's savepoint rolled back that
--    block only, and the next batch picked it again and failed again.
--
-- This generation keeps the signature, the candidate query, the thresholds
-- and the result shape, and changes two lines:
--   * v_matched_title is TEXT: the whole title comes back. The result column
--     matched_title is VARCHAR without a length, as before.
--   * A nearest candidate at NaN counts as no candidate: clean, similarity 0,
--     no match — the rule of the exact check, which compares on the one row
--     its LIMIT 1 keeps. NaN sorts after every number, so a nearest
--     candidate at NaN means the checked block has zero norm or every
--     candidate has; a real candidate next to a zero-norm one is still
--     found. Both routes of the batch answer alike since this migration
--     (TestGuardRoutesAnswerAlike, TestGuardExactSourceMatchesFunction).
--
-- WHY IN THE FUNCTION, AND WHY THE BLOCK IS STILL CHECKED. The title defect
-- raises inside the function, so it needs the function changed; the NaN rule
-- sits next to it, so the function itself answers what the exact statement
-- answers, for the batch and for anyone who calls it (a guard in Go would
-- have left the function answering NaN). Leaving a zero-norm block out of
-- the pick instead would keep it pending for good: counted by guard-stats on
-- every pass, an entry of idx_guard_pending_type (182) for ever, and the
-- pick's predicate is that index's predicate. Checked, it is recorded clean
-- and leaves the queue. No production writer stores such a vector:
-- embed.Embed normalizes and its quality gate refuses a norm outside
-- [0.99, 1.01], and the three writers of the vector columns
-- (store.StoreEmbedding, StoreEmbeddingIfUnchanged, StoreEmbeddingNext) take
-- their vectors from it; a restore or a manual write can.
--
-- COST. The candidate query and its plan are unchanged (the batch pins the
-- HNSW plan with enable_seqscan, enable_bitmapscan and enable_sort off for
-- the call, issue #381); the new work is one comparison on the one row the
-- query returns, and the title assignment loses its length check. Measured
-- before and after on the same index (docs/operations.md, "Migration 188").
--
-- LOCKS. CREATE OR REPLACE FUNCTION updates the function's pg_proc row and
-- takes no lock on a table: a guard batch in progress and a writer holding
-- context_blocks rows do not block it and are not blocked by it (measured:
-- 7 ms next to an open guard transaction that ran the old body and holds a
-- block row, TestMigration188ReplacesTheFunctionInPlace). A call that is
-- running keeps the body it started with; the next transaction on a backend
-- that compiled the old body runs the new one. A second instance running
-- this file at the same moment (the runner runs each file in one
-- transaction) waits for the first one's uncommitted row of the function:
-- `SET LOCAL lock_timeout = '3s'` (house pattern of 179/180) ends the wait
-- with 55P03, and a first run that commits inside it leaves the second one
-- "tuple concurrently updated" (XX000) — either way that boot fails loudly
-- and the next one finds 188 applied (TestMigration188ConcurrentRuns).
--
-- OLDER BINARY (the rollback images v5.68.0 and v5.69.0). It calls the
-- function with the same five parameters and reads the same six columns,
-- whose types are unchanged (CREATE OR REPLACE refuses another result
-- type), so its statement and a copy of it prepared before the migration
-- keep working (TestMigration188ReplacesTheFunctionInPlace); it gets the new
-- answers on its index route. Its exact statement is its own SQL and still coerces the
-- title to VARCHAR(255), as before. Its migration runner leaves version 188
-- alone. Its schema-contract check finds _migrations:188 without an
-- embedded file (breaking) and a body hash drift (param) for
-- ctx_guard_check: in mode warn (the default) it starts, logs them and
-- /health reports schema_contract attention; in mode enforce it refuses to
-- start until CTX_CONTRACT_MODE=warn or off is set.
--
-- WAY BACK (forward-only tree; to undo temporarily): first stop every
-- instance of a release that embeds this file — its runner applies every
-- embedded file whose _migrations row is missing, so a restart after the
-- DELETE would run this file again. Then run the CREATE OR REPLACE FUNCTION
-- ctx_guard_check block of 173_embedding_stale.sql, then
-- DELETE FROM _migrations WHERE version = 188, and only then start the
-- rollback image.
--
-- No table, column, index or trigger changes (test.sh T07 counts stay). The
-- schema-contract manifest records the new function definition and
-- migration_max 187 -> 188. Idempotent, forward-only. Registration by the
-- runner (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

-- ctx_guard_check, generation of 173 with a TEXT title and the NaN rule.
CREATE OR REPLACE FUNCTION ctx_guard_check(
    p_block_id            UUID,
    p_threshold_duplicate REAL,                   -- Pflicht (01 §3.6, kein Default)
    p_threshold_review    REAL,                   -- Pflicht (01 §3.6, kein Default)
    p_candidate_types     TEXT[],                 -- Pflicht; NULL ⇒ 0 Kandidaten (fail-closed)
    p_same_scope_only     BOOLEAN DEFAULT FALSE   -- Achse-02-Anteil (02 §4.7, K1/I-J)
)
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
    v_scope         VARCHAR(50);
    v_tenant        UUID;
    v_matched_id    UUID;
    v_matched_title TEXT;           -- the column's type: any length (188)
    v_matched_scope VARCHAR(50);
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

    -- The tenant that owns the block's scope (Modell C: one scope, one
    -- tenant). NULL for a scope registered to no tenant ⇒ the candidate
    -- predicate below degenerates to same-scope only (fail-closed).
    SELECT ts.tenant_id INTO v_tenant
    FROM context_tenant_scopes ts
    WHERE ts.scope = v_scope;

    -- Filtered ANN (02 §4.7): iterate the HNSW graph until the predicates
    -- yield candidates instead of returning a silent false-clean. Always on
    -- since 156 — the tenant predicate filters every call. LOCAL: resets at
    -- transaction end.
    SET LOCAL hnsw.iterative_scan = 'relaxed_order';

    -- Find Top-1 nearest neighbor (excluding self, excluding archived,
    -- excluding chunks, excluding stale vectors since 173) — restricted to
    -- the POLICY candidate set (p_candidate_types allowlist; NULL ⇒
    -- `= ANY(NULL)` never true ⇒ 0 candidates, fail-closed), to the OWN
    -- TENANT's scopes (156) and, for the issue axis, to the own scope.
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
      AND NOT cb.embedding_stale
      AND cb.lifecycle_state != 'chunk'
      AND cb.type_name = ANY(p_candidate_types)
      AND (NOT p_same_scope_only OR cb.scope = v_scope)
      AND (
            cb.scope = v_scope
         OR (v_tenant IS NOT NULL AND cb.scope IN (
                SELECT ts2.scope FROM context_tenant_scopes ts2 WHERE ts2.tenant_id = v_tenant))
      )
    ORDER BY cb.embedding::halfvec(1024) <=> v_embedding::halfvec(1024)
    LIMIT 1;

    -- No neighbors found — or the nearest one at NaN (188): a zero-norm
    -- vector has no cosine distance, and NaN passes every threshold. NaN
    -- sorts after every number, so the nearest candidate is at NaN only when
    -- the checked block has zero norm or every candidate has; either way
    -- there is no candidate to compare with.
    IF v_matched_id IS NULL OR v_similarity = 'NaN'::numeric THEN
        decision       := 'clean';
        top_similarity := 0;
        matched_id     := NULL;
        matched_title  := NULL;
        matched_scope  := NULL;
        is_cross_scope := false;
        RETURN NEXT;
        RETURN;
    END IF;

    -- Apply thresholds (policy parameters — the 011 literals left the body)
    IF v_similarity >= p_threshold_duplicate THEN
        decision := 'near_duplicate';
    ELSIF v_similarity >= p_threshold_review THEN
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
