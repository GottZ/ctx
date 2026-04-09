-- 022_stats_indexes.sql — Composite indexes for GetStats + ListMeta at 1M+ scale.
CREATE INDEX IF NOT EXISTS idx_context_scope_active
    ON context_blocks(scope, created_at DESC) WHERE NOT is_archived;
