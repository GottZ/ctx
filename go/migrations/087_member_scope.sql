-- =============================================================================
-- 087_member_scope.sql — denormalized scope on graph_cluster_member (B-W2)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Per-tenant overview line (overnight plan B): the rebuild loop (B-W6) tears
-- down and re-aggregates ONE tenant's partition per run. The scope-scoped
-- DELETE and the scope-scoped aggregation (B-W3) both need the member's scope
-- without joining context_blocks mid-teardown — so the member row carries it,
-- denormalized from the Louvain INPUT at insert time (persist writes what it
-- clustered, never a re-read: a concurrent block scope-move between load and
-- persist must not make the member row disagree with the partition it was
-- computed in).
--
-- Column type is TEXT — the 057 family convention (graph_cluster_node.scope,
-- graph_cluster_edge.scope_s/scope_t are TEXT; context_blocks.scope stays the
-- VARCHAR(50) source of truth, the join is type-compatible).
--
-- PK DECISION (lead review, load-bearing invariant): block_id remains the
-- SOLE primary key (057). That is correct exactly as long as the overview
-- input is strictly owned-disjoint — no grants in the Louvain input, so no
-- block can be a member under two scopes. B-W6 adds the input-purity
-- assertion; until then this header and the persist comment document the
-- invariant.
--
-- Backfill: existing rows get their block's current scope (FK ON DELETE
-- CASCADE guarantees every member has a live block, context_blocks.scope is
-- NOT NULL — so the backfill leaves no NULLs and SET NOT NULL is safe).
--
-- ROLLBACK NOTE (deliberate, 089/090 line): an old binary whose member INSERT
-- does not carry scope fails the NOT NULL constraint — the rebuild tx rolls
-- back LOUDLY and the previous overview tables stay readable (advisory-locked
-- replace, no partial state). Fail-loud beats a silent wrong-scope default:
-- any DEFAULT here would poison the B-W3 scope-scoped aggregation.
--
-- lock_timeout (R-MIG2): ADD COLUMN (nullable) + UPDATE on a <100k-row table
-- + SET NOT NULL take brief locks; the runner wraps the migration in one tx.
-- Idempotent: ADD COLUMN IF NOT EXISTS; the backfill UPDATE only touches NULL
-- rows; SET NOT NULL re-runs cleanly; _migrations INSERT ON CONFLICT DO
-- NOTHING. Forward-only. Additive column → test.sh table count UNCHANGED.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE graph_cluster_member
    ADD COLUMN IF NOT EXISTS scope TEXT;

UPDATE graph_cluster_member m
   SET scope = b.scope
  FROM context_blocks b
 WHERE b.id = m.block_id
   AND m.scope IS NULL;

ALTER TABLE graph_cluster_member
    ALTER COLUMN scope SET NOT NULL;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (87, '087_member_scope.sql', now())
  ON CONFLICT (version) DO NOTHING;
