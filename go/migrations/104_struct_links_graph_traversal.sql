-- =============================================================================
-- 104_struct_links_graph_traversal.sql — Ego-Traversal-Indizes (graph-structural GB1)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- M050 pattern (050:33-42) carried onto context_structural_links: seed column
-- leading, created_at DESC as the second key column (= free per-leg ordering,
-- Merge Append + early termination at the per-node LIMIT), INCLUDE payload
-- saves the heap fetch on the link row. created_at is the ordering-key stand-in
-- for the missing confidence column (conf 1.0 by definition, 076:8-9): "newest
-- reference wins the cap slot" (E5) — stable because PutStructuralLink
-- ON CONFLICT DO NOTHING never bumps created_at.
-- NO partial WHERE (unlike M050): structural has no supersedes.
--
-- Index balance after M104 = 4 B-trees (PK, idx_struct_links_scope,
-- graph_fwd, graph_rev; idx_struct_links_rev retires below, K4). Write amplification is
-- accepted for v1: the expected forge-sync bulk import is a batch/background
-- path, and ON CONFLICT DO NOTHING rejects re-puts without touching indexes.
-- CREATE INDEX without CONCURRENTLY is a no-op lock at today's volume; a
-- LATER re-deploy against a million-row table is caught by lock_timeout (the
-- migration fails instead of blocking) — CONCURRENTLY out-of-band is the
-- documented escape route (design/01 §3.2).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE INDEX IF NOT EXISTS idx_struct_links_graph_fwd
    ON context_structural_links (source_block_id, created_at DESC)
    INCLUDE (target_block_id, link_class, origin);

CREATE INDEX IF NOT EXISTS idx_struct_links_graph_rev
    ON context_structural_links (target_block_id, created_at DESC)
    INCLUDE (source_block_id, link_class, origin);

-- K4 consolidation (design/01 §3.2, decided by the GB1 EXPLAIN gate):
-- idx_struct_links_rev (076: target, link_class INCLUDE source) retires —
-- graph_rev above is also target-leading and serves its ONLY consumer (the
-- StructuralNeighbors reverse leg, structlinks.go) as an index scan with
-- link_class as a filter. Drop-probe evidence lives in
-- TestStructuralHop_IndexPlan (asserted, not just logged). Index balance
-- stays at 4 B-trees instead of 5.
DROP INDEX IF EXISTS idx_struct_links_rev;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (104, '104_struct_links_graph_traversal.sql', now())
  ON CONFLICT (version) DO NOTHING;
