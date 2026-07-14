-- =============================================================================
-- 106_graph_stat_indexes.sql — statistics/prune indexes for both link tables
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Plan graph-structural 2026-07-14, wave GD1 (design/04 §3, W04-4). Two
-- indexes, built NOW and in FINAL form while the tables are small: the
-- single-tx migration runner cannot CREATE INDEX CONCURRENTLY (N11 doctrine,
-- store/tenant.go), so a later build or shape change at 500k/3M rows would
-- lock the table for the build duration.
--
-- (1) idx_struct_links_scope_created — daily-report window statistics (GD2):
--     count per (link_class, origin) over [from, to) within the own scope.
--     The existing idx_struct_links_scope (076) covers only the scope column;
--     every window aggregation would heap-fetch all tenant rows. COVERING
--     (INCLUDE link_class, origin) because the query selects both: with the
--     INCLUDE the count is a true index-only scan (insert-only table —
--     PG13+ insert-triggered autovacuum keeps the visibility map fresh).
--     idx_struct_links_scope stays (dropping a live index is its own risk);
--     the fourth index's write amplification is µs-range for the three
--     writers (daily report, forge-sync bursts, manage API).
--
-- (2) idx_dream_links_scope_created — dream side of the same statistics
--     surface (GD3, scope-filtered window count) AND the N11 prune seam
--     (store/tenant.go names exactly this missing index for 1M+ prunes).
--     The prune rationale stands independently of GD3, so the index is
--     unconditional. Deliberately NOT covering: context_dream_links is
--     replace-swept on the hottest link-write path — an INCLUDE would be
--     write amplification for a daily heap-fetch of a few hundred rows.
--
-- Idempotent (IF NOT EXISTS + ON CONFLICT footer), pure indexes: no backfill,
-- no data change. Rollback = DROP INDEX. Deployable without an app change.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE INDEX IF NOT EXISTS idx_struct_links_scope_created
    ON context_structural_links (scope, created_at)
    INCLUDE (link_class, origin);

CREATE INDEX IF NOT EXISTS idx_dream_links_scope_created
    ON context_dream_links (scope, created_at);

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (106, '106_graph_stat_indexes.sql', now())
  ON CONFLICT (version) DO NOTHING;
