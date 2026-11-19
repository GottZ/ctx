-- =============================================================================
-- 182_guard_pending_type.sql — the guard pick reads the waiting blocks of the
-- checked types, not every unchecked block with a vector (issue #360)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- idx_guard_pending (113_baseline) is (created_at) WHERE NOT is_archived AND
-- (metadata->>'guard_checked_at') IS NULL AND embedding IS NOT NULL. The
-- guard pick's predicate (guard.guardPendingWhere) has four conditions more:
-- a vector that is not stale, a category other than index, the lifecycle
-- knowledge, and a type with guard.check = true in the registry. An unchecked
-- block with a vector that one of them filters out stays in that index for
-- good: blocks of an embedded type with guard.check = false (comment,
-- tool-evidence, tool-overview on the shipped registry), synthesis and
-- canonical blocks, category index, a stale vector until the backfill
-- replaces it. The pick walks the index by created_at and passed every such
-- entry older than the 100th block it took, on each run; manage guard-stats
-- counted over all of them. Measured on the production image (issue #360,
-- one million such entries beside one million checked blocks, live row
-- width): 169 402 buffers per pick with the heap in created_at order, about
-- one million with the heap in random order (live: correlation of
-- created_at 0.08).
--
-- This migration adds, BESIDE idx_guard_pending:
--
--   idx_guard_pending_type ON context_blocks
--       (type_name, timezone('UTC', created_at))
--       WHERE NOT is_archived
--         AND (metadata->>'guard_checked_at') IS NULL
--         AND embedding IS NOT NULL AND NOT embedding_stale
--         AND category <> 'index' AND lifecycle_state = 'knowledge'
--
-- The predicate holds every condition of the pick a catalog can hold. The
-- type allowlist comes from the registry: it is the leading key, so the
-- waiting blocks of a type with guard.check = false are entries no pick
-- reads. The second key is created_at as UTC wall time (guard.guardPickKey):
-- it orders as created_at does, and no other index carries it. The pick of
-- this release (guard.pickPending) reads one range of this index per checked
-- type in that key order, LIMIT the batch per type, in a savepoint with
-- sorts off for that statement; it keeps the oldest of all types, gives the
-- other locks back and locks the kept blocks again by row version. Ordered
-- by created_at itself, a key led by the type made the planner walk
-- idx_context_created through a large checked corpus instead (the first
-- build of #360, stopped); ordered by this key that walk does not exist as a
-- path, and with sorts off neither does a sort over idx_guard_pending. The
-- count (guard.pendingStatsSQL) is unchanged; the planner reads it from this
-- index or from idx_guard_pending. Measured in the same store as above: the
-- pick 15-18 buffers with nothing waiting, 315-319 to take 100 — flat in the
-- foreign entries and the checked blocks (docs/operations.md, "Migration
-- 182").
--
-- WHY idx_guard_pending STAYS. A binary before 182 (a rolling deploy, the
-- rollback image of this release) sends the pick ordered by created_at.
-- Beside a type-led index and without idx_guard_pending it walks
-- idx_context_created through the corpus (measured 214 327 buffers at
-- 1.2 million rows); with idx_guard_pending it keeps today's plan and cost.
-- So idx_guard_pending is dropped by a migration of a later release, whose
-- rollback image already speaks the pick of this one. Until then every
-- qualifying write maintains both indexes.
--
-- RE-EMBED. The predicate names the embedding column, so the index is in the
-- attnum class of the re-embed cutover: its twin idx_guard_pending_type_next
-- (events.verifySisterIndexes) carries this definition on embedding_next and
-- the swap renames it onto this name. A migration that reached verifying on
-- an older release has no such twin: the verify gate builds it, and on a
-- green verdict the migration cycle builds it alone (issue #333); the
-- confirm refuses until it stands.
--
-- INDEX BUILD AND ITS RUNBOOK (the 171 pattern). The runner runs each file in
-- ONE transaction (store/migrations.go), where CONCURRENTLY is impossible.
-- CREATE INDEX takes SHARE on context_blocks until the file commits: readers
-- (and row locks) go on, every INSERT, UPDATE and DELETE waits; the build
-- reads the whole heap, so its time follows the table, not the index.
--   * A valid index of this name with exactly this definition: no-op.
--   * Below 500 000 rows (pg_class.reltuples; never analysed: a bounded
--     count) and no index of this name: built inline. Measured on the
--     production image (rows of about 1.5 KB of heap, a writer inserting
--     every 2 ms; docs/operations.md, "Migration 182"): the longest wait of
--     that writer was 102-182 ms at 250 000 rows, 188-248 ms at 500 000 and
--     0.90-1.78 s at 1 000 000, which is why the inline build stops at
--     500 000.
--   * Above, or a name taken by anything else (another definition, or an
--     INVALID index — an aborted CONCURRENTLY build): WARNING, nothing built
--     or dropped, and no lock on context_blocks. Build it out of band BEFORE
--     the binary of this release serves such a corpus:
--
--       DROP INDEX CONCURRENTLY IF EXISTS idx_guard_pending_type;
--       CREATE INDEX CONCURRENTLY idx_guard_pending_type
--           ON context_blocks (type_name, timezone('UTC', created_at))
--           WHERE NOT is_archived
--             AND (metadata->>'guard_checked_at') IS NULL
--             AND embedding IS NOT NULL
--             AND NOT embedding_stale
--             AND category <> 'index'
--             AND lifecycle_state = 'knowledge';
--
--     (readers and writers go on; the DROP only removes an INVALID leftover
--     or a foreign index of this name — check what it is first). Until the
--     index stands with this definition the pick of this release sends its
--     statement before 182 (guard.guardPickIndexReadySQL), at the cost before
--     182, and the schema-contract manifest stays red.
--
-- `SET LOCAL lock_timeout = '3s'` (house pattern): a transaction that holds a
-- write on context_blocks longer fails the migration with lock_not_available
-- (55P03), the file rolls back with nothing changed and the boot fails
-- loudly; the next boot tries again.
--
-- OLDER BINARY. Its pick and count use idx_guard_pending as before (the
-- tests run its statements as literals on this schema). Its migration
-- runner leaves version 182 alone. Its schema-contract check finds
-- _migrations:182 without an embedded file and an index it does not know: in
-- mode warn (the default) it starts, logs them and /health reports
-- schema_contract attention; in mode enforce it refuses to start until
-- CTX_CONTRACT_MODE=warn or off is set, or the schema is taken back:
-- DROP INDEX CONCURRENTLY idx_guard_pending_type; then
-- DELETE FROM _migrations WHERE version = 182.
--
-- Index change only, no table or column: the test.sh T07 counts stay. The
-- schema-contract manifest gains the index and migration_max 181 -> 182.
-- Idempotent, forward-only. Registration by the runner (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

DO $do$
DECLARE
    c_def   CONSTANT TEXT := 'CREATE INDEX idx_guard_pending_type ON public.context_blocks USING btree (type_name, timezone(''UTC''::text, created_at)) WHERE ((NOT is_archived) AND ((metadata ->> ''guard_checked_at''::text) IS NULL) AND (embedding IS NOT NULL) AND (NOT embedding_stale) AND ((category)::text <> ''index''::text) AND ((lifecycle_state)::text = ''knowledge''::text))';
    v_def   TEXT;
    v_valid BOOLEAN;
    v_rows  BIGINT;
BEGIN
    SELECT pg_get_indexdef(c.oid), i.indisvalid INTO v_def, v_valid
      FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
      JOIN pg_index     i ON i.indexrelid = c.oid
     WHERE n.nspname = 'public' AND c.relname = 'idx_guard_pending_type';

    IF FOUND THEN
        -- The definition as PostgreSQL deparses it, so a same-named index on
        -- another table, key or predicate is not adopted.
        IF v_valid AND v_def = c_def THEN
            RETURN;                 -- usable, nothing to do
        END IF;
        RAISE WARNING 'idx_guard_pending_type exists but is not the guard pick index (valid=%): %. Nothing built or dropped; until it is replaced the guard pick runs its statement before 182, at its cost before 182. Runbook in the header of 182_guard_pending_type.sql; the schema-contract manifest stays red until then.',
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
        RAISE WARNING 'idx_guard_pending_type is missing at % rows: build it out of band before this release serves (CREATE INDEX CONCURRENTLY, runbook in the header of 182_guard_pending_type.sql). Until then the guard pick runs its statement before 182, at its cost before 182; the schema-contract manifest stays red.',
            v_rows;
        RETURN;
    END IF;

    CREATE INDEX idx_guard_pending_type
        ON context_blocks (type_name, timezone('UTC', created_at))
        WHERE NOT is_archived
          AND (metadata->>'guard_checked_at') IS NULL
          AND embedding IS NOT NULL
          AND NOT embedding_stale
          AND category <> 'index'
          AND lifecycle_state = 'knowledge';
END
$do$;
