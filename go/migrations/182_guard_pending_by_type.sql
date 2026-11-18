-- =============================================================================
-- 182_guard_pending_by_type.sql — the guard pick and guard-stats read the
-- blocks the guard takes, not every unchecked block with a vector (issue #360)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- idx_guard_pending (113_baseline) was (created_at) WHERE NOT is_archived AND
-- (metadata->>'guard_checked_at') IS NULL AND embedding IS NOT NULL. The
-- guard pick's predicate (guard.guardPendingWhere) has four conditions more:
-- a vector that is not stale, a category other than index, the lifecycle
-- knowledge, and a type with guard.check = true in the registry. An unchecked
-- block with a vector that one of them filters out stayed in the index for
-- good: blocks of an embedded type with guard.check = false (comment,
-- tool-evidence, tool-overview on the shipped registry), synthesis and
-- canonical blocks, category index, a stale vector until the backfill
-- replaces it. The pick walks the index by created_at and passed every such
-- entry older than the 100th block it took, on each run; manage guard-stats
-- (migration 181) counted over all of them. Measured on the production image
-- (issue #360, foreign entries older than the waiting blocks): the pick of an
-- empty queue read 585 buffers beside 10 000 such entries, 5 832 beside
-- 100 000 and 61 233 (0.6-0.7 s) beside 1 000 000 — about 0.06 buffers per
-- entry, on up to 60 picks an hour while blocks are written.
--
-- Since this migration the index is
--
--   (type_name, created_at) WHERE NOT is_archived
--       AND (metadata->>'guard_checked_at') IS NULL
--       AND embedding IS NOT NULL AND NOT embedding_stale
--       AND category <> 'index' AND lifecycle_state = 'knowledge'
--
-- Its predicate holds every condition of the pick a catalog can hold. The
-- type allowlist comes from the registry, which a predicate cannot read: it
-- is the leading key instead, so the pick's type_name = ANY($1) is the index
-- condition, and the entries of a type with guard.check = false stay in the
-- index without a pick or a count reading them. The pick and the count
-- (guard.guardPickSQL, guard.pendingStatsSQL) are unchanged: their predicate
-- implies this one, the predicate has no parameter, so a custom and a
-- generic plan both use the index (TestGuardPendingCostBesideForeignEntries).
-- A guard.check switch takes effect on the next run (the allowlist is a bind
-- parameter of the registry snapshot); a re-type or a scope move is index
-- maintenance. No column, trigger or queue.
--
-- ORDER AND PRICE. The key no longer gives created_at order over all types,
-- so the pick reads the waiting entries of the checked types and sorts them:
-- the batch is still the oldest blocks over all checked types, created_at
-- ascending (TestGuardPickTwoRunsOldestFirstOverAllTypes, also with runs side
-- by side under SKIP LOCKED). The sort reads every waiting entry, not only
-- the 100 it takes: measured 109 buffers to take 100 of 100 waiting blocks,
-- about 1 300 for 100 of 10 000 (0.12 per waiting block); the backlog shrinks
-- as the guard catches up. Beside 1 000 000 foreign entries the pick read 3
-- buffers with none waiting and 109 with 100 waiting, the count 9.
--
-- LIFECYCLE. The predicate writes "only the lifecycle knowledge is checked"
-- into the index, as the pick has done since migration 074. Checking another
-- lifecycle later means rebuilding this index.
--
-- RE-EMBED. The predicate names the embedding column, so the index is in the
-- attnum class of the re-embed cutover: its twin idx_guard_pending_next
-- (events.verifySisterIndexes) carries this definition on embedding_next, and
-- the swap renames it onto this name. A twin of the definition before this
-- migration, built by a re-embed migration that reached verifying on an
-- older release, is rebuilt by the verify gate, or on a green verdict by the
-- migration cycle alone (issue #333); the confirm refuses until it stands.
--
-- INDEX BUILD AND ITS RUNBOOK (the 176 pattern). The runner runs each file in
-- ONE transaction (store/migrations.go), where CONCURRENTLY is impossible,
-- and every lock the file takes is held to its commit:
--   * A valid index of this name with exactly this definition: no-op.
--   * Below 500 000 rows (pg_class.reltuples), the index before this
--     migration (valid or not), an INVALID index of this definition (an
--     aborted CONCURRENTLY build under this name) or none: the file drops it
--     and builds this definition inline. The DROP is the file's first lock on
--     context_blocks (on a table never analysed a bounded count reads it
--     first, ACCESS SHARE), ACCESS EXCLUSIVE, so a transaction that has read
--     the table and then writes it goes ahead of the waiting DROP instead of
--     closing a deadlock with it (TestMigration182, reader_that_writes). From
--     the DROP to the commit no other session reads or writes context_blocks:
--     they wait, and one with a lock_timeout fails with 55P03. The build
--     reads the whole heap. Measured on the production image (rows of about
--     2 KB of heap, 5 % with a vector, warm cache, three rounds, load 7-11 on
--     12 cores), the longest wait of a writer that inserted every 2 ms during
--     the rebuild: 33-41 ms at 100 000 rows (205 MB heap), 70-114 ms at
--     250 000 (512 MB), 152-184 ms at 500 000 (1.0 GB); 0.73-0.98 s at
--     1 000 000 (2.0 GB, load 21), which is why the inline build stops at
--     500 000. A cold cache reads the heap from disk once (not measured).
--   * Above, or a name taken by anything else: WARNING, nothing built or
--     dropped, and no lock on context_blocks. Build out of band, at any time,
--     before or after this binary serves:
--
--       CREATE INDEX CONCURRENTLY idx_guard_pending_360
--           ON context_blocks (type_name, created_at)
--           WHERE NOT is_archived
--             AND (metadata->>'guard_checked_at') IS NULL
--             AND embedding IS NOT NULL
--             AND NOT embedding_stale
--             AND category <> 'index'
--             AND lifecycle_state = 'knowledge';
--       DROP INDEX CONCURRENTLY idx_guard_pending;
--       ALTER INDEX idx_guard_pending_360 RENAME TO idx_guard_pending;
--
--     (abortable; an aborted build leaves an INVALID index, which DROP INDEX
--     removes before the next try; the rename takes SHARE UPDATE EXCLUSIVE,
--     readers and writers go on). Measured with CONCURRENTLY at 1 200 000
--     rows: 0.55-2.1 s (issue #360, under load). Until the rename the pick and
--     the count run on the index before this migration at its cost, or on
--     the new one under the temporary name, and the schema-contract manifest
--     stays red.
--
-- `SET LOCAL lock_timeout = '3s'` (house pattern): a transaction holding
-- context_blocks longer fails the migration with lock_not_available (55P03),
-- the file rolls back with nothing changed and the boot fails loudly; the
-- next boot tries again.
--
-- OLDER BINARY (rolling deploy, rollback). Its pick and count are the
-- statements of this binary and use this index in custom and generic plans
-- (the pins run them as literals). Its migration runner leaves version 182
-- alone. Its schema-contract check finds _migrations:182 without an
-- embedded file and the definition of idx_guard_pending changed: in mode
-- warn (the default) it starts, logs them and /health reports
-- schema_contract attention; in mode enforce it refuses to start until
-- CTX_CONTRACT_MODE=warn or off is set, or the index is taken back: the
-- runbook above with the definition before this migration
-- ((created_at) WHERE NOT is_archived AND (metadata->>'guard_checked_at') IS
-- NULL AND embedding IS NOT NULL) under a temporary name, then
-- DELETE FROM _migrations WHERE version = 182.
--
-- Index change only, no table or column: the test.sh T07 counts stay. The
-- schema-contract manifest records the new definition of idx_guard_pending
-- and migration_max 181 -> 182. Idempotent, forward-only. Registration by the
-- runner (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

DO $do$
DECLARE
    c_new   CONSTANT TEXT := 'CREATE INDEX idx_guard_pending ON public.context_blocks USING btree (type_name, created_at) WHERE ((NOT is_archived) AND ((metadata ->> ''guard_checked_at''::text) IS NULL) AND (embedding IS NOT NULL) AND (NOT embedding_stale) AND ((category)::text <> ''index''::text) AND ((lifecycle_state)::text = ''knowledge''::text))';
    c_old   CONSTANT TEXT := 'CREATE INDEX idx_guard_pending ON public.context_blocks USING btree (created_at) WHERE ((NOT is_archived) AND ((metadata ->> ''guard_checked_at''::text) IS NULL) AND (embedding IS NOT NULL))';
    v_found BOOLEAN;
    v_def   TEXT;
    v_valid BOOLEAN;
    v_rows  BIGINT;
BEGIN
    SELECT pg_get_indexdef(c.oid), i.indisvalid INTO v_def, v_valid
      FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
      JOIN pg_index     i ON i.indexrelid = c.oid
     WHERE n.nspname = 'public' AND c.relname = 'idx_guard_pending';
    v_found := FOUND;

    IF v_found AND v_valid AND v_def = c_new THEN
        RETURN;                 -- usable, nothing to do
    END IF;
    IF v_found AND v_def <> c_old AND v_def <> c_new THEN
        RAISE WARNING 'idx_guard_pending is not the guard pending index (valid=%): %. Nothing built or dropped; the guard pick and guard-stats stay correct on the indexes they find. Rename or drop it, then build the index of migration 182 out of band (runbook in the header of 182_guard_pending_by_type.sql); the schema-contract manifest stays red until then.',
            v_valid, v_def;
        RETURN;
    END IF;

    SELECT c.reltuples::BIGINT INTO v_rows
      FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'public' AND c.relname = 'context_blocks';
    IF v_rows IS NULL OR v_rows < 0 THEN
        -- reltuples < 0: never analysed. A bounded count, never a full scan.
        SELECT count(*) INTO v_rows FROM (SELECT 1 FROM context_blocks LIMIT 500000) t;
    END IF;

    IF v_rows >= 500000 THEN
        RAISE WARNING 'idx_guard_pending is not the index of migration 182 at % rows (valid=%): %. Build it out of band (CREATE INDEX CONCURRENTLY, runbook in the header of 182_guard_pending_by_type.sql). The guard pick and guard-stats stay correct at their cost before 182; the schema-contract manifest stays red until then.',
            v_rows, v_valid, COALESCE(v_def, 'missing');
        RETURN;
    END IF;

    -- The first lock of the file on context_blocks that it holds to its
    -- commit: ACCESS EXCLUSIVE (INDEX BUILD AND ITS RUNBOOK in the header).
    DROP INDEX IF EXISTS idx_guard_pending;
    CREATE INDEX idx_guard_pending
        ON context_blocks (type_name, created_at)
        WHERE NOT is_archived
          AND (metadata->>'guard_checked_at') IS NULL
          AND embedding IS NOT NULL
          AND NOT embedding_stale
          AND category <> 'index'
          AND lifecycle_state = 'knowledge';
END
$do$;
