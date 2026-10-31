-- =============================================================================
-- 156_guard_tenant_candidates.sql — the duplicate guard never looks across
-- tenants
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- ctx_guard_check (baseline 113, last generation at the M074/T7 cut) picked
-- the nearest neighbour of a new block among the blocks of EVERY tenant: the
-- candidate query narrowed to the block's own scope only when the type's
-- policy asked for same-scope candidates (the issue axis), and the built-in
-- knowledge/reference/audit-trail/system-meta policies use candidates=all.
-- The decision then wrote guard_similarity, guard_matched_id and
-- guard_is_cross_scope into the checked block's metadata and, at the
-- duplicate threshold, archived the checked block — so any member key of any
-- tenant had a confirm/deny oracle on other tenants' content (store a guess,
-- read the similarity and the matched UUIDv7 back with `get`) and a tenant's
-- legitimate writes could be auto-archived as duplicates of a foreign
-- tenant's block (GHSA 2026-10-31 §2, reported privately).
--
-- This generation keeps the signature, the thresholds, the same-scope axis
-- and the result shape byte-identical and adds ONE predicate: candidates are
-- bounded to the scopes owned by the checked block's tenant
-- (context_tenant_scopes, Modell C — a scope belongs to exactly one tenant).
-- A block whose scope is registered to no tenant (a system or legacy scope)
-- gets same-scope candidates only (fail-closed). Cross-scope matching INSIDE
-- a tenant (private vs work of the same tenant, the guard_is_cross_scope flag)
-- is unchanged. The HNSW iterative scan is now enabled for every call, not
-- only the same-scope axis: the tenant predicate is a filtered ANN for every
-- block, and a small tenant would otherwise get a silent false-clean when the
-- first ef_search candidates all belong to other tenants.
--
-- Data repair, idempotent: a block archived as a duplicate of a block that
-- lives in ANOTHER tenant is un-archived and returned to the pending state
-- (guard metadata cleared, guard_status back to the DDL default) so the next
-- guard batch re-checks it against its own tenant only. Matched blocks that
-- no longer exist cannot be attributed and are left as they are.

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
    -- excluding chunks) — restricted to the POLICY candidate set
    -- (p_candidate_types allowlist; NULL ⇒ `= ANY(NULL)` never true ⇒ 0
    -- candidates, fail-closed), to the OWN TENANT's scopes (156) and, for the
    -- issue axis, to the own scope.
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
      AND (
            cb.scope = v_scope
         OR (v_tenant IS NOT NULL AND cb.scope IN (
                SELECT ts2.scope FROM context_tenant_scopes ts2 WHERE ts2.tenant_id = v_tenant))
      )
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

-- Data repair: un-archive blocks that were archived as duplicates of a block
-- in another tenant, and hand them back to the guard (pending = no
-- guard_checked_at, see guardPendingWhere). Idempotent — a second run finds
-- no such row.
UPDATE context_blocks b
   SET is_archived  = false,
       guard_status = 'active',
       metadata     = (COALESCE(b.metadata, '{}'::jsonb)
                       - 'guard_status' - 'guard_checked_at' - 'guard_similarity'
                       - 'guard_matched_id' - 'guard_is_cross_scope'
                       - 'guard_threshold_duplicate' - 'guard_threshold_review'),
       updated_at   = now()
 WHERE b.is_archived
   AND b.guard_status = 'archived_dup'
   AND EXISTS (
         SELECT 1
           FROM context_blocks m
           JOIN context_tenant_scopes mts ON mts.scope = m.scope
           LEFT JOIN context_tenant_scopes bts ON bts.scope = b.scope
          WHERE m.id::text = b.metadata->>'guard_matched_id'
            AND (bts.tenant_id IS NULL OR bts.tenant_id <> mts.tenant_id)
   );
