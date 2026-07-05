-- =============================================================================
-- 088_meta_scope_pk.sql — graph_overview_meta singleton → scope PK (B-W5)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Per-tenant overview line (overnight plan B): the 057 meta table was a
-- single row ("last rebuild"); with per-partition rebuilds (B-W3/B-W6) each
-- scope carries its own computed_at — the read path answers "how fresh is MY
-- overview" as max(computed_at) over the caller's readScopes, never another
-- tenant's timestamp (leak B1-m1). Writer and reader change in the SAME wave
-- (masterplan B-W5: schema + read are non-divisible).
--
-- DATA MIGRATION (B3-M2): the existing singleton row is rewritten to one row
-- per REAL scope (DISTINCT scope FROM graph_cluster_node) with its
-- computed_at preserved — the boot-time rebuild check (overviewNeverBuilt =
-- zero rows) keeps its verdict: previously-built stays "built" (no spurious
-- boot rebuild), never-built stays empty. Degenerate case: a meta row exists
-- but graph_cluster_node is empty (meta without data) ⇒ zero rows after the
-- migration and the boot rebuild fires — correct there, it rebuilds what the
-- meta row only pretended to describe.
--
-- Sentinel note (B3-M1): rows carry REAL scopes only — no sentinel scope, no
-- computed_at:null window. The transition-phase writer (cluster.go, this
-- wave) derives its scope set from graph_cluster_node in the same tx.
--
-- Also here (B-W3 finding): graph_cluster_member(scope) gets an index — the
-- scoped teardown DELETE (and the scoped aggregation joins) would otherwise
-- seq-scan a 1M+ member table on every per-tenant rebuild.
--
-- edge_n semantics change with the per-scope rows: it counts INTRA-partition
-- edge rows (scope_s = scope_t = scope). Cross-scope rows from a pre-B global
-- run belong to no single partition and appear in no meta row; the global
-- node_n/cluster_n sums are recoverable as sum() over rows.
--
-- lock_timeout (R-MIG2): ALTER/DROP COLUMN, the data move (1 row → N scopes,
-- N = live scope count) and CREATE INDEX on the <100k member table take brief
-- locks; the runner wraps the whole file in one tx (atomic).
-- Idempotent: ADD/DROP COLUMN IF (NOT) EXISTS; the data-move INSERT only
-- fires while a scope-less row exists; SET NOT NULL and the guarded ADD
-- PRIMARY KEY re-run cleanly; CREATE INDEX IF NOT EXISTS; _migrations INSERT
-- ON CONFLICT DO NOTHING. Forward-only. No new table → test.sh count
-- UNCHANGED.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE graph_overview_meta
    ADD COLUMN IF NOT EXISTS scope TEXT;

-- Dropping the singleton column drops the old PK and its CHECK with it —
-- required BEFORE the data move (two singleton=true rows cannot coexist).
ALTER TABLE graph_overview_meta
    DROP COLUMN IF EXISTS singleton;

-- Data move (B3-M2): duplicate the scope-less legacy row onto every real
-- scope, preserving computed_at + stats; then retire the legacy row.
INSERT INTO graph_overview_meta (scope, computed_at, modularity, cluster_n, node_n, edge_n, resolution)
SELECT s.scope, m.computed_at, m.modularity, m.cluster_n, m.node_n, m.edge_n, m.resolution
  FROM graph_overview_meta m
 CROSS JOIN (SELECT DISTINCT scope FROM graph_cluster_node) AS s(scope)
 WHERE m.scope IS NULL
   AND NOT EXISTS (SELECT 1 FROM graph_overview_meta e WHERE e.scope = s.scope);

DELETE FROM graph_overview_meta WHERE scope IS NULL;

ALTER TABLE graph_overview_meta
    ALTER COLUMN scope SET NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
         WHERE conrelid = 'graph_overview_meta'::regclass AND contype = 'p'
    ) THEN
        ALTER TABLE graph_overview_meta ADD PRIMARY KEY (scope);
    END IF;
END $$;

-- B-W3 finding: the scoped teardown DELETE needs this at 1M+ member rows.
CREATE INDEX IF NOT EXISTS idx_gcm_scope ON graph_cluster_member (scope);

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (88, '088_meta_scope_pk.sql', now())
  ON CONFLICT (version) DO NOTHING;
