-- =============================================================================
-- 050_dream_links_graph_traversal.sql — covering indexes for query-time
-- Dream-graph traversal (GottZ Graph Expansion, Wave 1)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Wave 1: a new post-RRF Go stage (rrf.GraphExpand) 1-hop-expands the top RRF
-- seeds along the four POSITIVE Dream link types (topical/factual/causal/
-- recurrent) and fuses the neighbors via a Go boost. The negative 'supersedes'
-- type stays out — it is consumed by filterSuperseded as a drop-filter, never
-- as a traversal edge.
--
-- The traversal does ONE batched edge-fetch keyed by seed id, in BOTH directions
-- (inbound: target_block_id = ANY(seeds); outbound: source_block_id = ANY(seeds))
-- with a per-edge gate on raw_confidence and an ORDER BY raw_confidence DESC for
-- the per-seed cap (ROW_NUMBER()). No covering index existed for that access
-- pattern: idx_dream_links_target (M016) covers only the target column, and the
-- forward direction had only the primary key (source_block_id, target_block_id).
--
-- Two composite PARTIAL COVERING indexes — one per direction — make the gate +
-- per-seed-cap a pure index scan: the leading column is the seed side, the
-- second column gives the raw_confidence DESC ordering for free, and the
-- INCLUDE payload (the opposite block id + relationship + weighted confidence)
-- lets the planner satisfy the window/cap without a heap fetch on the link row.
-- The partial predicate (relationship <> 'supersedes') keeps the indexes lean —
-- they only carry the four traversed types.
--
-- Additive + idempotent: CREATE INDEX IF NOT EXISTS. The graph stage itself is
-- gated default-OFF in Go (GraphConfig.Enabled), so this migration is pure
-- infrastructure — it changes no query behavior on its own.
-- =============================================================================

-- Outbound traversal: source_block_id = ANY(seeds), ordered by raw_confidence.
CREATE INDEX IF NOT EXISTS idx_dream_links_graph_fwd
    ON context_dream_links (source_block_id, raw_confidence DESC)
    INCLUDE (target_block_id, relationship, confidence)
    WHERE relationship <> 'supersedes';

-- Inbound traversal: target_block_id = ANY(seeds), ordered by raw_confidence.
CREATE INDEX IF NOT EXISTS idx_dream_links_graph_rev
    ON context_dream_links (target_block_id, raw_confidence DESC)
    INCLUDE (source_block_id, relationship, confidence)
    WHERE relationship <> 'supersedes';

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (50, '050_dream_links_graph_traversal.sql', now())
  ON CONFLICT (version) DO NOTHING;
