-- =============================================================================
-- 157_guard_leftovers_repair.sql — guard decisions on live blocks that name a
-- match the tenant-bounded guard could not have found
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Before 156 the duplicate guard searched the nearest neighbour of a new block
-- among the blocks of every tenant and wrote guard_similarity,
-- guard_matched_id and guard_is_cross_scope into the checked block's
-- metadata. 156 bounded the candidates to the checked block's tenant and
-- repaired the rows the old guard had ARCHIVED against another tenant's
-- block. Every other row kept its decision keys whatever has happened to it
-- since — a review flag, a clean verdict, a block a person kept through
-- guard-resolve, a block that has become canonical — and with them the
-- foreign similarity and the foreign UUIDv7 (§F-§2). The read paths redact
-- guard_matched_id when the caller cannot read the matched block; the
-- similarity, the cross-scope flag and a flag raised against the foreign
-- block stay visible, and the decision is one the current guard would never
-- make.
--
-- Which rows: live rows (NOT is_archived, any guard_status, any lifecycle)
-- whose guard_matched_id names an existing block that the candidate
-- predicate of ctx_guard_check (156) excludes — the exact inverse of
-- `cb.scope = v_scope OR (v_tenant IS NOT NULL AND cb.scope IN <the tenant's
-- scopes>)`. That is a match in another tenant, a match outside the own scope
-- when the own scope is registered to no tenant, and a match in a scope
-- registered to no tenant. It contains the tenant predicate of the 156 repair
-- (which joined the matched scope's tenant and therefore skipped the last
-- case) and adds that case, which the old guard produced as well. Matched
-- blocks that no longer exist cannot be attributed and are left as they are,
-- as in 156; the read paths already treat them as unreadable.
--
-- What it does depends on whether a person has decided on the row:
--   (a) No guard-resolve trace (no metadata.guard_resolution): the guard's
--       decision is handed back. guard_status returns to the DDL default
--       'active' and every guard decision key goes, guard_checked_at among
--       them, so guardPendingWhere picks the row up again and the next guard
--       batch re-checks it against its own tenant only — where the guard
--       still checks the row at all: a row decided while it was 'knowledge'
--       that is 'canonical' or a snapshot now is not pending and simply keeps
--       no decision.
--   (b) A guard-resolve trace (keep or archive — any decision of a person):
--       the decision stands. Only the keys that point at the foreign block
--       go — guard_matched_id, guard_similarity, guard_is_cross_scope;
--       guard_status, guard_checked_at, the thresholds and the trace stay,
--       so the row is not pending and the guard does not put it up for a
--       verdict again (a re-check could archive a kept block as the
--       near duplicate of a block of its own tenant).
-- Both branches set metadata.guard_repair = 'M157' (a reserved key,
-- derived.ReservedMetadataKeys; 121 and 156 write it with their own number),
-- so `metadata->>'guard_repair' = 'M157'` reconstructs the touched set later.
--
-- The match is compared as uuid, so context_blocks_pkey serves the probe.
-- pg_input_is_valid keeps an empty or malformed guard_matched_id (the guard
-- writes '' when it found no match) away from the cast, which would raise
-- 22P02 and roll the boot back; the CASE, not an AND, guarantees that order.
--
-- No row is un-archived, so neither partial unique index over live rows
-- (uq_context_category_title_scope, uq_source_chunk) can be hit, and one
-- statement suffices. Idempotent: both branches remove guard_matched_id, so a
-- second run matches nothing.
UPDATE context_blocks b
   SET guard_status = CASE WHEN b.metadata ? 'guard_resolution'
                           THEN b.guard_status ELSE 'active' END,
       metadata     = CASE WHEN b.metadata ? 'guard_resolution'
                           THEN b.metadata
                                - 'guard_matched_id' - 'guard_similarity' - 'guard_is_cross_scope'
                           ELSE COALESCE(b.metadata, '{}'::jsonb)
                                - 'guard_status' - 'guard_checked_at' - 'guard_similarity'
                                - 'guard_matched_id' - 'guard_is_cross_scope'
                                - 'guard_threshold_duplicate' - 'guard_threshold_review'
                      END || jsonb_build_object('guard_repair', 'M157'),
       updated_at   = now()
 WHERE NOT b.is_archived
   AND b.metadata ? 'guard_matched_id'
   AND EXISTS (
         SELECT 1
           FROM context_blocks m
           LEFT JOIN context_tenant_scopes mts ON mts.scope = m.scope
           LEFT JOIN context_tenant_scopes bts ON bts.scope = b.scope
          WHERE m.id = CASE WHEN pg_input_is_valid(b.metadata->>'guard_matched_id', 'uuid')
                            THEN (b.metadata->>'guard_matched_id')::uuid END
            AND m.scope <> b.scope
            AND (bts.tenant_id IS NULL OR mts.tenant_id IS DISTINCT FROM bts.tenant_id));
