-- =============================================================================
-- 083_block_type_refcount_index.sql — index the block-type reference count.
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Workflow-engine axis 03, wave W2 (design/03 §4.2 DELETE row / §6 target
-- scale). W2 exposes DELETE /api/types/{name}; its reference guard
-- (store.DeleteBlockType) runs
--
--     SELECT count(*) FILTER (WHERE NOT is_archived),
--            count(*) FILTER (WHERE is_archived)
--       FROM context_blocks WHERE type_name = $1
--
-- to turn a still-referenced type into a 409 + count. context_blocks.type_name
-- (renamed from block_role in 071) carries NO index: idx_block_type has indexed
-- lifecycle_state since 070 renamed the OLD block_type column out from under it.
-- At the 1M+-blocks/tenant target scale an unindexed WHERE type_name = $1 is a
-- seq-scan on every type delete, so the guard would degrade linearly with the
-- corpus. A plain btree on type_name makes it index-supported.
--
-- Plain CREATE INDEX (not CONCURRENTLY): the migration runner wraps every file
-- in ONE transaction (store/migrations.go), where CONCURRENTLY is illegal — the
-- established convention for every index migration in this tree (001/002/022).
-- The build takes a brief ACCESS SHARE-blocking lock on context_blocks; at the
-- current corpus it is instant, at target scale it is a maintenance-window op.
-- Forward-only, idempotent, self-registering (069 pattern).
-- =============================================================================

CREATE INDEX IF NOT EXISTS idx_blocks_type_name ON context_blocks(type_name);

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (83, '083_block_type_refcount_index.sql', now())
  ON CONFLICT (version) DO NOTHING;
