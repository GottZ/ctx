-- =============================================================================
-- 074_guard_check_type_policy.sql — ctx_guard_check parametrisiert + Guard-Pending-Index
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Workflow-engine axis 01, wave T7 (design/01-type-registry.md §3.6) MERGED
-- with the axis-02 K1 share (design/02-issue-workflow.md §4.7/§9.1): ONE
-- unified ctx_guard_check generation in ONE DROP+CREATE — never two.
--
-- Axis 01 share (§3.6): thresholds + candidate set become PARAMETERS fed from
-- the block-type registry (M072). All three are MANDATORY — NO defaults:
--
--   * p_threshold_duplicate / p_threshold_review — the 0.98/0.92 literals of
--     the 011 line leave the function body; Go resolves them per BLOCK TYPE
--     (blocktype.Set.GuardThresholds).
--   * p_candidate_types — the candidate allowlist (guard.candidate=true
--     types). NULL ⇒ 0 candidates (`= ANY(NULL)` is never true — hard,
--     analogous to p_types_visible in 073). A DEFAULT NULL with "legacy =
--     all types" would be a silent policy bypass (§5.4 actionTier lesson);
--     a legacy 1-arg call therefore fails LOUDLY with 42883.
--
-- Axis 02 share (02 §4.7 — the K1 Issue-Guard order):
--
--   * p_same_scope_only — TRUE restricts the candidate set to the checked
--     block's own scope (issue dedup must never match cross-tenant/scope,
--     02 §5.3). DEFAULT FALSE: 02 §4.7 orders the default explicitly (its
--     "1-arg rollback" rationale predates the 01-R1 no-defaults decision and
--     is void — the 1-arg call is 42883 now — but the FALSE default remains
--     correct: cross-scope stays the knowledge-line semantic, and Go passes
--     the parameter EXPLICITLY at every call site regardless; the I-J gate
--     enumerates call sites against <5-arg calls).
--   * Same-scope branch sets hnsw.iterative_scan='relaxed_order' (filtered
--     ANN, 02 §4.7): at 1M+ blocks a ≤1%-selectivity scope predicate can
--     exhaust the ef_search candidates without a single same-scope hit —
--     the guard would silently report clean. relaxed_order makes pgvector
--     iterate the graph until the predicate yields candidates (house
--     pattern: ctx_rrf since 003/006, last 068/073). The cross-scope branch
--     keeps the 011-line unfiltered top-1 (no GUC) — byte-identical
--     behaviour under seed defaults.
--
-- Body deltas vs 070 (the 011 line): candidate filter gains
-- `cb.lifecycle_state != 'chunk'` (NOT-NULL since 070 — the NULL arm of the
-- old predicate is dead) AND `cb.type_name = ANY(p_candidate_types)` AND the
-- optional scope conjunct; the threshold CASEs consume the parameters.
-- v_scope/v_matched_scope widened VARCHAR(20)→VARCHAR(50): scope has been
-- VARCHAR(50) since 047 — the old declarations were a latent 22001
-- truncation error for long scopes, never a behaviour change for short ones.
--
-- idx_guard_pending (§3.6 R1 — the missing guard-pending index): the pick
-- predicate `(metadata->>'guard_checked_at') IS NULL` (guard.go) and the two
-- count subqueries had NO carrying index — GIN(metadata) (jsonb_ops) does not
-- serve `->> IS NULL`, and idx_context_created only carries the ORDER BY
-- (heap-filtering every row). At 1M+ blocks that is 2-3 full scans per guard
-- batch, and forge-sync bursts (10k+ issues/repo) trigger batches via
-- NotifyWrite exactly then. Partial index after the idx_dream_pending pattern
-- (016): only pending rows are IN the index, so it stays small while the
-- table grows. Predicate = the three conjuncts all three guard queries share;
-- category != 'index', the type allowlist and (T12) the scope conjunct filter
-- on the index result.
--
-- Function + index only, no table/column change: test.sh T07 table/column
-- counts unchanged. Idempotent (DROP IF EXISTS both signatures + CREATE +
-- IF NOT EXISTS + ON CONFLICT DO NOTHING), forward-only, self-registering
-- (M031+ convention).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE INDEX IF NOT EXISTS idx_guard_pending
    ON context_blocks (created_at ASC)
    WHERE NOT is_archived
      AND (metadata->>'guard_checked_at') IS NULL
      AND embedding IS NOT NULL;

-- DROP+CREATE (house pattern 068:48-50, 02 §9.1): CREATE OR REPLACE with a
-- changed parameter list would leave the 1-arg overload alive → 42725
-- ambiguity; the old signature must FALL so the legacy call is a loud 42883.
DROP FUNCTION IF EXISTS ctx_guard_check(UUID);
DROP FUNCTION IF EXISTS ctx_guard_check(UUID, REAL, REAL, TEXT[], BOOLEAN);

CREATE FUNCTION ctx_guard_check(
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
    v_matched_id    UUID;
    v_matched_title VARCHAR(255);
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

    -- Same-scope dedup is filtered ANN (02 §4.7): iterate the HNSW graph
    -- until the scope predicate yields candidates instead of returning a
    -- silent false-clean. LOCAL: resets at transaction end; the cross-scope
    -- branch deliberately sets nothing (011-line plan, byte-identical).
    IF p_same_scope_only THEN
        SET LOCAL hnsw.iterative_scan = 'relaxed_order';
    END IF;

    -- Find Top-1 nearest neighbor (excluding self, excluding archived,
    -- excluding chunks) — restricted to the POLICY candidate set
    -- (p_candidate_types allowlist; NULL ⇒ `= ANY(NULL)` never true ⇒ 0
    -- candidates, fail-closed) and, for the issue axis, to the own scope.
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
      AND cb.lifecycle_state != 'chunk'
      AND cb.type_name = ANY(p_candidate_types)
      AND (NOT p_same_scope_only OR cb.scope = v_scope)
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

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (74, '074_guard_check_type_policy.sql', now())
  ON CONFLICT (version) DO NOTHING;
