-- =============================================================================
-- 191_migration_rows.sql — a migration decides "inline or runbook" by the
-- current size of a table, not by the estimate of its last ANALYZE (issue #399)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Twelve files build an index or validate a constraint inline below a row
-- threshold and leave it to an out-of-band runbook above it: 115, 140, 171,
-- 172, 173, 175, 176, 182, 184, 185, 186 and 187. Each decides by a copy of
-- one query: pg_class.reltuples, and only for a table never analysed
-- (reltuples < 0) a count stopped at the threshold. reltuples is the row
-- count of the last VACUUM or ANALYZE; a table that grew since (autovacuum
-- off for it or for the cluster, or a bulk load autovacuum has not reached
-- yet) passes as small, and so does a table analysed while empty or with
-- every row dead and filled since (reltuples 0). Measured for the issue
-- (PostgreSQL 18.6): the access tokens analysed at 100 000 rows and grown to
-- 3 000 000, file 184 built both indexes inline and held SHARE on
-- context_access_tokens for 1.08 s; the write log analysed at 1 000 000 and
-- grown to 10 000 000, file 185 held SHARE on context_block_grants and
-- context_oauth_codes for 6.45 s. Those files are frozen (a changed byte is
-- a checksum mismatch on every database that ran them). This file gives the
-- next migration with a threshold a function to ask instead of a query to
-- copy.
--
-- ctx_migration_rows(p_rel regclass, p_threshold bigint) RETURNS bigint is
-- the size a migration compares with its threshold: at or above it, the
-- out-of-band path. It measures rows, not the heap (THE HEAP, below).
--
-- AN ESTIMATE DECIDES ONLY "OUT OF BAND". For each relation with a heap
-- among p_rel and the tree under it in pg_inherits (its partitions, its
-- inheritance children, the chunks of a TimescaleDB hypertable) the function
-- takes the largest of
--   * reltuples, the stored estimate;
--   * reltuples / relpages x pg_relation_size / block_size, the stored
--     density applied to the current size of the heap (its main fork) —
--     for the issue's tokens 100 000 / 3 125 x 93 750 = 3 000 000;
--   * n_live_tup, the live rows the cumulative statistics count;
-- and sums them. A sum, rounded up, at or above p_threshold is the answer:
-- the file goes out of band without reading the table. Below it the function
-- counts the rows through p_rel and stops at p_threshold, so no estimate
-- ever decides "inline". The answer is the exact row count below the
-- threshold; at or above it, the estimate, or p_threshold itself where a
-- count stopped there.
--
-- WHY NO ESTIMATE MAY SAY "BELOW". All three measures can trail the table
-- together. VACUUM sets reltuples and n_live_tup by extrapolation
-- (vac_estimate_reltuples): the pages it skips as all-visible count at the
-- stored density of the whole table, and the density times the size inherits
-- the same reltuples. 100 000 rows made all-visible, every row beyond the
-- first 10 000 but one in a thousand deleted, ANALYZE (10 090, right),
-- VACUUM: all three read 1 103, since the 10 000 rows sit on the pages
-- VACUUM skipped; refilled to 100 000 into the freed space, 91 013.
-- Retention (the oldest 90 % deleted, ANALYZE, VACUUM): 1 048 for 10 000.
-- VACUUM only, never ANALYZE (half deleted, VACUUM, refilled, VACUUM):
-- 75 030 for 100 000. A refill whose counts a live session still holds back
-- (a session flushes them with a delay, and at its end): 2 001 for
-- 20 000; deletes whose counts reach the statistics after an ANALYZE that saw
-- them are subtracted twice. Each of these is now counted, and the count
-- says above (TestMigration191EstimateBelowIsCounted). The count reads at
-- most p_threshold live rows (on the issue's tokens 204 ms for 500 000 rows,
-- 226 ms for 1 000 000). It runs wherever no estimate says above: before an
-- inline build, which reads the whole heap anyway, and also where the count
-- itself then reaches the threshold and the file goes out of band.
--
-- WHY THE THREE MEASURES. They let a table above the threshold go out of
-- band without a read, and they give the WARNING a size near the truth. The
-- density times the size follows growth of the heap's file; n_live_tup
-- follows growth the file does not show (2 000 rows left of 20 000,
-- refilled to 20 000 in the same 409 pages: n_live_tup 20 000, the others
-- 2 000); reltuples covers a heap packed denser than its stored statistics
-- (relation statistics restored onto it). Their errors upwards cost an
-- out-of-band path the table did not need: dead row versions count as live
-- in the density times the size (2 000 live rows updated twice read as
-- 6 000), n_live_tup counts an insert twice whose counts reach the
-- statistics after an ANALYZE that saw it, and reltuples stays high after a
-- mass delete until the next VACUUM or ANALYZE.
--
-- WHERE IT COUNTS ANYWAY. No sum is formed, the function counts, when a
-- relation of the tree has a heap and
--   * reltuples -1: never analysed, and after TRUNCATE;
--   * reltuples 0: analysed while empty or with every row dead;
--   * relpages below 32 (c_density_pages; 0 included): a density read from
--     so few pages is too coarse to claim "above" (900 short rows analysed
--     on five pages, grown with 2 000 wide ones: about 90 000 for 2 900);
--   * no VACUUM and no ANALYZE of it on record in the cumulative statistics:
--     crash recovery, pg_stat_reset and pg_upgrade drop them, and with them
--     the count n_live_tup continues from (track_counts off never records
--     them).
-- A heap of zero pages holds no row: an empty table or a partitioned parent
-- adds nothing.
--
-- WHAT THE COUNT DOES NOT SEE. It counts the rows the caller sees:
--   * not the rows of a transaction still open when it runs: the build that
--     follows waits for that transaction's lock (at most the file's
--     lock_timeout, then 55P03 and the next boot) and then reads them;
--   * not the rows row-level security hides from a role subject to it (a
--     non-owner without BYPASSRLS, or the owner under FORCE ROW LEVEL
--     SECURITY): no table of ctx has row-level security, and ctx migrates as
--     the owner of its tables.
--
-- THE HEAP. The function measures rows. An index build or a validation reads
-- every page of the heap, dead or empty, so a heap far larger than its rows
-- (mass deletes or updates) makes an inline file read its size, not its
-- rows: 10 000 000 rows of which 5 are live (1 655 MB of heap) count as 5 —
-- 3.7 s for the first read after the delete, which sets the hint bits the
-- delete left as any first reader does, then 0.17 to 0.22 s — and an inline
-- build over them took 2.1 s, 0.21 s after a VACUUM. VACUUM empties such
-- pages but returns them only where they trail the heap (that VACUUM left
-- the 1 655 MB). Before the upgrade that runs such a file, compact a
-- bloated table out of band (pg_repack, or VACUUM FULL in a maintenance
-- window) or build the file's index out of band by its runbook
-- (docs/development.md, "A migration with a row threshold").
--
-- TOAST. pg_relation_size(oid) is the main fork. A value stored out of line
-- is a pointer in the main heap, reltuples / relpages is rows per page of
-- the main heap, and the main heap is what an index build reads: TOAST
-- belongs in neither side of the product.
--
-- HYPERTABLES. None of the twelve files measures one (context_llm_log and
-- context_pending_writes, the hypertables of ctx, are behind no threshold).
-- The parent's pg_class row carries reltuples 0 or -1 and no heap, the
-- chunks carry the numbers: the copied query reads 0 there and builds inline
-- at any size; the function sums the chunks. A compressed chunk keeps its
-- rows in an internal hypertable outside the tree: the sum reads low, a sum
-- below the threshold is counted (the count reads through the compression),
-- and a sum at or above it is right to go out of band. On a hypertable under
-- writes the newest chunk is mostly not analysed yet, so the function
-- counts; that count plans every chunk and holds ACCESS SHARE on each to the
-- commit (one lock slot per chunk of max_locks_per_transaction).
--
-- USAGE (docs/development.md, "A migration with a row threshold"):
--     v_rows := ctx_migration_rows('public.context_blocks', 500000);
--     IF v_rows >= 500000 THEN RAISE WARNING '...'; RETURN; END IF;
-- Each of the twelve files would have said its decision in that one line
-- (TestMigration191IssueMeasurement rebuilds 172 and 184 so and replays the
-- issue's measurement at a fiftieth of its size). A name without a relation
-- fails at the regclass cast (42P01), an oid without one in the function
-- (42P01); a view, an index, a sequence, a materialized view or a foreign
-- partition fails with 42809, a missing or non-positive threshold with
-- 22023: never a 0 a caller would read as small. From this file on, a
-- migration whose code decides a size itself (reltuples, relpages,
-- n_live_tup, pg_stat_get_live_tuples, pg_relation_size,
-- pg_total_relation_size or pg_table_size outside a definition of this
-- function) fails TestMigrationsDecideSizeWithTheRowsFunction
-- (go/migrations).
--
-- LOCKS AND COST. The function reads pg_class, pg_inherits and the
-- cumulative statistics (no table lock) and calls pg_relation_size once per
-- relation of the tree, which takes ACCESS SHARE on that relation and
-- releases it before returning: it waits only behind a holder, or a queued
-- requester, of ACCESS EXCLUSIVE (bounded by the caller's lock_timeout) and
-- holds nothing afterwards. The count reads up to p_threshold live rows
-- under ACCESS SHARE held to the end of the caller's transaction, as any
-- read and as the twelve files did for a table never analysed; in a heap of
-- mostly dead rows it reads on until it has p_threshold live rows or reaches
-- the end. Measured at the issue's 3 000 000 tokens: the call 0.6 ms
-- (docs/operations.md, "Migration 191").
--
-- SECURITY. Invoker rights, no SECURITY DEFINER: it reads catalogs and
-- statistics every role may read, and its count needs the caller's SELECT
-- on the table. SET search_path = pg_catalog, pg_temp, the manual's form for
-- a function that names nothing of its own schema: pg_catalog first, pg_temp
-- named last, so a temporary table cannot stand in for pg_class or
-- pg_inherits. The relation reaches the count as regclass text, quoted and
-- qualified by its output function. VOLATILE (the default), not STABLE: it
-- reads the current size of a file, which another session's insert grows
-- between two calls in one statement (pg_relation_size is volatile for that
-- reason).
--
-- THE TWELVE FILES STAY. A fresh install runs them on empty tables (reltuples
-- -1, a count of no rows): nothing to do. A database upgraded from before
-- one of them at a size near or above its threshold decides by reltuples:
-- ANALYZE the tables of the files ahead right before the upgrade. ANALYZE
-- samples the table; a VACUUM alone extrapolates and can trail it, as above
-- (docs/operations.md, "Migration 191").
--
-- MIGRATION. CREATE OR REPLACE FUNCTION writes the function's catalog row
-- and takes no lock on a table (TestMigration191Locks runs this file beside
-- ACCESS EXCLUSIVE holders of every table the twelve files measure).
-- `SET LOCAL lock_timeout = '3s'` (house pattern) bounds the wait for a
-- session that replaces the same function at the same moment.
--
-- OLDER BINARY. The rollback image (v5.73.0) never calls the function. Its
-- schema-contract check finds _migrations:191 without an embedded file
-- (migration_integrity, breaking) and a function its manifest does not know
-- (unknown_active_object, breaking): in contract mode warn (the default) it
-- starts with /health reporting schema_contract attention; in mode enforce
-- it refuses to start until CTX_CONTRACT_MODE=warn or off is set.
--
-- WAY BACK (forward-only tree; only while no later migration calls the
-- function, and after every instance of a release that embeds 191 has
-- stopped, whose runner would apply the file again):
--     DROP FUNCTION ctx_migration_rows(regclass, bigint);
--     DELETE FROM _migrations WHERE version = 191;
--
-- No table, column, index or trigger: test.sh T07 still counts 71 tables and
-- 45 columns. The schema-contract manifest gains the function and
-- migration_max 190 -> 191. Idempotent, forward-only. Registration by the
-- runner (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel regclass, p_threshold bigint)
RETURNS bigint
LANGUAGE plpgsql
VOLATILE
SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    -- A density read from fewer heap pages is too coarse to claim "above".
    c_density_pages CONSTANT INTEGER := 32;
    v_node     RECORD;
    v_pages    DOUBLE PRECISION;
    v_estimate DOUBLE PRECISION := 0;
    v_count    BOOLEAN := false;     -- count the rows instead of estimating
    v_rows     BIGINT;
BEGIN
    IF p_rel IS NULL OR p_threshold IS NULL OR p_threshold < 1 THEN
        RAISE EXCEPTION 'ctx_migration_rows(%, %): a relation and a threshold of at least 1 are required',
            p_rel, p_threshold USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_class WHERE oid = p_rel) THEN
        RAISE EXCEPTION 'ctx_migration_rows: no relation has the oid %', p_rel::oid
            USING ERRCODE = 'undefined_table';
    END IF;

    -- The relation and every partition, inheritance child and chunk below it.
    FOR v_node IN
        WITH RECURSIVE tree(oid) AS (
            SELECT p_rel::oid
            UNION
            SELECT i.inhrelid FROM pg_inherits i JOIN tree t ON i.inhparent = t.oid
        )
        SELECT c.oid, c.relkind, c.reltuples, c.relpages
          FROM tree JOIN pg_class c ON c.oid = tree.oid
    LOOP
        IF v_node.relkind NOT IN ('r', 'p') THEN
            RAISE EXCEPTION 'ctx_migration_rows: % is not a table (relkind %)', v_node.oid::regclass, v_node.relkind
                USING ERRCODE = 'wrong_object_type';
        END IF;
        -- The main fork; a relation dropped since the tree was read has none.
        v_pages := COALESCE(pg_relation_size(v_node.oid), 0) / current_setting('block_size')::double precision;
        CONTINUE WHEN v_pages = 0;
        IF v_node.reltuples <= 0 OR v_node.relpages < c_density_pages
           OR COALESCE(pg_stat_get_last_vacuum_time(v_node.oid), pg_stat_get_last_autovacuum_time(v_node.oid),
                       pg_stat_get_last_analyze_time(v_node.oid), pg_stat_get_last_autoanalyze_time(v_node.oid)) IS NULL
        THEN
            v_count := true;                 -- no estimate to trust
        ELSE
            v_estimate := v_estimate + greatest(v_node.reltuples::double precision,
                                                v_node.reltuples::double precision / v_node.relpages * v_pages,
                                                pg_stat_get_live_tuples(v_node.oid)::double precision);
        END IF;
    END LOOP;

    -- An estimate decides "out of band" only: below the threshold, count.
    IF NOT v_count AND ceil(v_estimate) >= p_threshold THEN
        RETURN ceil(v_estimate)::bigint;
    END IF;
    EXECUTE format('SELECT count(*) FROM (SELECT 1 FROM %s LIMIT %s) t', p_rel, p_threshold) INTO v_rows;
    RETURN v_rows;
END
$fn$;

COMMENT ON FUNCTION ctx_migration_rows(regclass, bigint) IS
    'Rows of a table for a migration''s inline-or-runbook threshold: an estimate at or above the threshold (the largest of the stored estimate, the stored density times the current heap and the live-row counter, summed over partitions, inheritance children and hypertable chunks), else a count stopped at the threshold (migration 191, issue #399).';
