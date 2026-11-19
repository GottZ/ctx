-- =============================================================================
-- 183_guard_pending_dropped.sql — the guard pending index of 113 and its
-- re-embed twin are dropped once the pick index of 182 stands (issue #360)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- idx_guard_pending (113_baseline) is (created_at) WHERE NOT is_archived AND
-- (metadata->>'guard_checked_at') IS NULL AND embedding IS NOT NULL. Since
-- migration 182 the guard pick reads idx_guard_pending_type, and manage
-- guard-stats (guard.pendingStatsSQL) reads it too once idx_guard_pending is
-- gone; 182 kept idx_guard_pending for the binaries before it, whose pick
-- orders by created_at and walks idx_context_created through the store
-- without it (214 327 buffers per pick at 1.2 million rows, issue #360). The
-- rollback image of the release of this file is v5.66.0, whose pick is the
-- pick of 182. Until this file runs every write of an unchecked block with a
-- vector maintains both indexes, and the count may read idx_guard_pending,
-- whose entries include every unchecked block with a vector the pick filters
-- out (docs/operations.md, "Migration 183").
--
-- The file drops, each only when it is the index ctx built under that name
-- on context_blocks (the deparsed definition; any validity):
--   idx_guard_pending       only while idx_guard_pending_type stands as 182
--                           builds it (valid, ready, the definition of 182 —
--                           the probe the pick itself runs,
--                           guard.guardPickIndexReadySQL). Otherwise it stays
--                           with a WARNING: without idx_guard_pending_type
--                           the pick of this release sends its statement
--                           before 182, which orders by created_at, and that
--                           statement reads idx_guard_pending or walks the
--                           store (measured beside 200 000 checked blocks:
--                           docs/operations.md, "Migration 183").
--   idx_guard_pending_next  the re-embed twin of idx_guard_pending on
--                           embedding_next (verify gate of a binary before
--                           this release); this release neither builds nor
--                           renames it (events.verifySisterIndexes,
--                           events.cutoverIndexRenames), so after a swap it
--                           would stay as a second copy on the serving column.
--   idx_guard_pending_old   the leftover of a swap, on embedding_old, before
--                           its cleanup; this release's cleanup and rollback
--                           no longer name it (events.cutoverOldIndexNames).
-- A same-named index with another definition (an object ctx did not build)
-- is left alone with a WARNING.
--
-- LOCKS. DROP INDEX takes ACCESS EXCLUSIVE on context_blocks, readers
-- included, and holds it until this file commits (the 181 pattern: a catalog
-- change, no row is read or written; the file of the index is removed at the
-- commit). The time held does not grow with the table — measured 1-5 ms at
-- 250 000, 500 000 and 1 000 000 rows (docs/operations.md, "Migration 183")
-- — so there is no row threshold; a threshold bounds a build that reads the
-- heap, and this file reads none. To get the lock it waits for
-- every transaction that has touched the table and for every holder of SHARE
-- UPDATE EXCLUSIVE (VACUUM, ANALYZE, CREATE INDEX CONCURRENTLY); while it
-- waits, every later statement on the table queues behind it. `SET LOCAL
-- lock_timeout = '3s'` (house pattern) bounds that wait: a holder that stays
-- longer fails the migration with lock_not_available (55P03), the file rolls
-- back with nothing changed and the boot fails loudly; the next boot tries
-- again. With nothing to drop the file takes no lock on context_blocks.
--
-- RUNBOOK (WARNING "idx_guard_pending is kept ..."). In this order, out of
-- band, readers and writers go on:
--   1. build idx_guard_pending_type (the runbook in the header of
--      182_guard_pending_type.sql), and check it:
--        SELECT indisvalid, indisready, pg_get_indexdef(indexrelid)
--          FROM pg_index WHERE indexrelid = 'idx_guard_pending_type'::regclass;
--   2. then, and only then: DROP INDEX CONCURRENTLY idx_guard_pending;
-- The reverse order leaves the pick's fallback without its index. An
-- operator who wants no ACCESS EXCLUSIVE at the boot runs both steps before
-- the upgrade (and DROP INDEX CONCURRENTLY IF EXISTS idx_guard_pending_next,
-- idx_guard_pending_old); the file then finds nothing to drop.
--
-- OLDER BINARY. v5.66.0 (the rollback image; its guard, re-embed and
-- schema-contract code is that of v5.65.0, which the tests ran on this
-- schema) picks and counts from idx_guard_pending_type as this release does.
-- Its verify gate and index repair still build idx_guard_pending_next for a
-- re-embed migration in verifying — during a rolling deploy an instance of it
-- builds the twin again after this file dropped it, and this release never
-- drops it again; once no older instance runs, drop it before the swap
-- (DROP INDEX CONCURRENTLY IF EXISTS idx_guard_pending_next), or it stays as
-- a second copy of the old index on the serving column. Its re-embed cutover
-- still renames idx_guard_pending, which no longer exists: its
-- confirm (the swap), its rollback of a swap and its cleanup fail with nothing
-- changed (finish or abort a re-embed migration with this release). A binary
-- before 182 walks the store with each pick on this schema: none may serve
-- it. The schema-contract check of an older binary finds _migrations:183
-- without an embedded file and idx_guard_pending missing (both breaking): in
-- mode warn (the default) it starts and /health reports schema_contract
-- attention; in mode enforce it refuses to start until CTX_CONTRACT_MODE=warn
-- or off is set (every later migration of this release adds a _migrations
-- row of its own, so the WAY BACK below alone does not satisfy enforce).
--
-- WAY BACK: CREATE INDEX CONCURRENTLY idx_guard_pending ON context_blocks
-- (created_at) WHERE NOT is_archived AND (metadata->>'guard_checked_at') IS
-- NULL AND embedding IS NOT NULL; then DELETE FROM _migrations WHERE version =
-- 183.
--
-- Index change only, no table or column: the test.sh T07 counts stay. The
-- schema-contract manifest loses idx_guard_pending and records migration_max
-- 182 -> 183. Idempotent, forward-only. Registration by the runner
-- (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

DO $do$
DECLARE
    -- idx_guard_pending_type as 182 builds it, the string the pick compares
    -- (guard.guardPickIndexReadySQL).
    c_type CONSTANT TEXT := 'CREATE INDEX idx_guard_pending_type ON public.context_blocks USING btree (type_name, timezone(''UTC''::text, created_at)) WHERE ((NOT is_archived) AND ((metadata ->> ''guard_checked_at''::text) IS NULL) AND (embedding IS NOT NULL) AND (NOT embedding_stale) AND ((category)::text <> ''index''::text) AND ((lifecycle_state)::text = ''knowledge''::text))';
    v_ready BOOLEAN;
    v_ix    RECORD;
    v_def   TEXT;
BEGIN
    SELECT EXISTS (SELECT 1 FROM pg_index
                    WHERE indexrelid = to_regclass('public.idx_guard_pending_type')
                      AND indisvalid AND indisready
                      AND pg_get_indexdef(indexrelid) = c_type)
      INTO v_ready;

    FOR v_ix IN
        SELECT * FROM (VALUES
            (1, 'idx_guard_pending_next', 'embedding_next'),
            (2, 'idx_guard_pending_old',  'embedding_old'),
            (3, 'idx_guard_pending',      'embedding')
        ) AS t(ord, name, col)
        ORDER BY ord
    LOOP
        SELECT pg_get_indexdef(c.oid) INTO v_def
          FROM pg_class c
          JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'public' AND c.relname = v_ix.name AND c.relkind = 'i';
        IF NOT FOUND THEN
            CONTINUE;
        END IF;
        IF v_def <> format('CREATE INDEX %s ON public.context_blocks USING btree (created_at) WHERE ((NOT is_archived) AND ((metadata ->> ''guard_checked_at''::text) IS NULL) AND (%s IS NOT NULL))', v_ix.name, v_ix.col) THEN
            RAISE WARNING '% is not the guard pending index ctx built under that name: %. Left as it is; the schema-contract manifest reports it until it is renamed or dropped (runbook in the header of 183_guard_pending_dropped.sql).',
                v_ix.name, v_def;
            CONTINUE;
        END IF;
        IF v_ix.name = 'idx_guard_pending' AND NOT v_ready THEN
            RAISE WARNING 'idx_guard_pending is kept: idx_guard_pending_type does not stand as migration 182 builds it, and the guard pick sends its statement before 182, which reads idx_guard_pending. Build idx_guard_pending_type (runbook of 182_guard_pending_type.sql), then DROP INDEX CONCURRENTLY idx_guard_pending (runbook in the header of 183_guard_pending_dropped.sql); the schema-contract manifest reports idx_guard_pending until then.';
            CONTINUE;
        END IF;
        EXECUTE format('DROP INDEX IF EXISTS %I', v_ix.name);
    END LOOP;
END
$do$;
