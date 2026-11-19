-- =============================================================================
-- 186_blocks_created_at_not_null.sql — context_blocks.created_at and
-- updated_at are NOT NULL (issue #237)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Both columns have been nullable since migration 001 (folded into
-- 113_baseline.sql: TIMESTAMPTZ DEFAULT now()). Every list read scans
-- created_at into a time.Time since #217, and the block read (store.Block,
-- get) scans both: a row without one fails compact search (browse and full
-- text), recent and get for every page that contains it ("cannot scan NULL
-- into *time.Time"). No write path produces such a row: no INSERT into
-- context_blocks names created_at, each gets the default, every UPDATE that
-- writes updated_at writes now(), and no UPDATE writes created_at. Only a
-- manual write does.
--
-- PostgreSQL 18 records NOT NULL as a constraint of its own that can be added
-- NOT VALID and validated later. This file
--   1. adds context_blocks_created_at_not_null (the name ALTER COLUMN ... SET
--      NOT NULL gives it) NOT VALID, unless the column has a not-null
--      constraint already: a catalog change without a scan. From then on an
--      insert or update that leaves created_at NULL is refused with 23502;
--   2. repairs the rows without created_at (REPAIR);
--   3. below 500 000 rows (pg_class.reltuples; never analysed: a bounded
--      count) adds context_blocks_updated_at_not_null NOT VALID the same way
--      and validates both constraints inline, in one ALTER TABLE; above, it
--      WARNs and leaves updated_at and the validation to the operator
--      (RUNBOOK).
-- The end state is the one of ALTER COLUMN ... SET NOT NULL on both columns:
-- the same pg_constraint rows (contype n, validated, local, inheritable).
--
-- The issue proposed CHECK (created_at IS NOT NULL) NOT VALID, VALIDATE, SET
-- NOT NULL (which skips its scan when a valid CHECK proves the column), then
-- DROP CONSTRAINT. It ends in the same state and takes the same locks; the
-- native form needs no temporary constraint.
--
-- WHY updated_at, AND WHY ONLY INLINE. store.Block scans updated_at into a
-- time.Time as well, no row of the reference instance lacks it, and the
-- inline validation checks both columns in the one heap scan it makes for
-- created_at (measured: docs/operations.md, "Migration 186"). What differs
-- is the repair: no index holds updated_at, so its rows without a value
-- cannot be found without that scan. Inline the file validates first and
-- repairs updated_at only when the validation fails on such a row (23502):
-- then one scan more, under the same lock, and the validation again. Above
-- the threshold it leaves updated_at alone: a NOT VALID constraint there
-- would refuse every later update of a row still without updated_at that does
-- not write the column (the guard and dream batches do not), until someone
-- repairs it.
--
-- REPAIR. A row without created_at gets COALESCE(uuid_extract_timestamp(id),
-- updated_at, now()). The id default is uuidv7(), whose timestamp is the
-- millisecond of the insert: on the reference instance every id is version 7
-- and lies within 16 ms of created_at. A row with another id version gets
-- updated_at, else the time of this migration. The rows are found through
-- idx_context_created (created_at DESC, a full btree, which holds NULLs), a
-- few buffers whatever the size of the table. A row without updated_at gets
-- COALESCE(created_at, uuid_extract_timestamp(id), now()). Each repair
-- names one column, which no BEFORE UPDATE OF trigger of context_blocks
-- names, so none fires and the other column stays; the row-level AFTER
-- trigger (block-write notify) fires per repaired row, the counter triggers
-- add no delta (no key changes). A NOTICE names the number of repaired rows
-- and the first 20 ids. It runs after step 1, under its ACCESS EXCLUSIVE
-- lock, so no row without a value can appear between the repair and the
-- commit. Failing instead would stop every boot on such a database until
-- someone edits the rows by hand, for a value the id carries.
--
-- LOCKS. Step 1 takes ACCESS EXCLUSIVE on context_blocks, readers included,
-- and the runner runs each file in ONE transaction (store/migrations.go), so
-- the lock is held to the commit and an inline VALIDATE scans the heap with
-- readers and writers waiting. The threshold keeps that wait under a second
-- (measured with a writer and a reader on second connections, rows of the
-- live width: docs/operations.md, "Migration 186"); a cold cache reads the
-- heap once. Above it the file holds the lock for the catalog change and the
-- index-driven repair only. The index idx_guard_pending_type of 182 keys
-- timezone('UTC', created_at); a not-null constraint rewrites no table and
-- rebuilds no index, so it stays as it is. `SET LOCAL lock_timeout = '3s'`
-- (house pattern) bounds the wait for the lock, not the time it is held: a
-- transaction that holds context_blocks longer fails the migration with
-- 55P03, the file rolls back with nothing changed, the next boot tries again.
--
-- RUNBOOK above the threshold (WARNING "context_blocks.created_at carries
-- ..."). Every new row is checked and no row is without created_at; what is
-- missing is the proof on the old rows and the constraint on updated_at.
-- Run, out of band, at any time, in this order:
--
--     UPDATE context_blocks
--        SET updated_at = COALESCE(created_at, uuid_extract_timestamp(id), now())
--      WHERE updated_at IS NULL;
--     ALTER TABLE context_blocks
--         ADD CONSTRAINT context_blocks_updated_at_not_null NOT NULL updated_at NOT VALID;
--     ALTER TABLE context_blocks
--         VALIDATE CONSTRAINT context_blocks_created_at_not_null,
--         VALIDATE CONSTRAINT context_blocks_updated_at_not_null;
--
-- The UPDATE scans the heap with row locks only (readers and writers go on).
-- The ADD is a catalog change under ACCESS EXCLUSIVE (milliseconds, after the
-- wait for the lock). The VALIDATE takes SHARE UPDATE EXCLUSIVE: readers and
-- writers go on; it waits for (and blocks) VACUUM, ANALYZE, CREATE INDEX
-- CONCURRENTLY and another VALIDATE. Should it report a row without
-- updated_at (one written by hand between the UPDATE and the ADD), repeat
-- the UPDATE and the VALIDATE. Running this file again afterwards is a
-- no-op; it can also run before the upgrade (ADD both NOT VALID in one
-- transaction, the VALIDATE in a second), and the file then finds valid
-- not-null constraints and changes nothing.
--
-- SCHEMA CONTRACT. The manifest records not_null true for both columns and
-- migration_max 185 -> 186; above the threshold updated_at reads as drift
-- (param) until the runbook ran. An older binary (v5.65.0, the rollback
-- image) finds _migrations:186 without an embedded file (breaking) and
-- not_null true where its manifest says false (definition drift, param): in mode warn (the default) it starts and
-- /health reports schema_contract attention; in mode enforce it refuses to
-- start (every _migrations row without an embedded file is breaking) until
-- CTX_CONTRACT_MODE=warn or off is set.
-- Its writes do not name created_at and write updated_at as now(), and keep
-- working.
--
-- WAY BACK (forward-only tree; to undo temporarily):
--     ALTER TABLE context_blocks ALTER COLUMN created_at DROP NOT NULL,
--                                ALTER COLUMN updated_at DROP NOT NULL;
-- (drops the constraints, a catalog change), then DELETE FROM _migrations
-- WHERE version = 186.
--
-- No table or column is added or removed: the test.sh T07 counts stay.
-- Idempotent, forward-only. Registration by the runner (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

DO $do$
DECLARE
    v_pending   TEXT[] := '{}';
    v_con       TEXT;
    v_valid     BOOLEAN;
    v_upd_con   TEXT;
    v_upd_valid BOOLEAN;
    v_rows      BIGINT;
    v_fixed     BIGINT;
    v_ids       UUID[];
BEGIN
    -- The not-null constraint of each column, if any, and whether it is
    -- validated.
    SELECT c.conname, c.convalidated INTO v_con, v_valid
      FROM pg_constraint c
      JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
     WHERE c.conrelid = 'public.context_blocks'::regclass
       AND c.contype = 'n' AND a.attname = 'created_at';
    SELECT c.conname, c.convalidated INTO v_upd_con, v_upd_valid
      FROM pg_constraint c
      JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
     WHERE c.conrelid = 'public.context_blocks'::regclass
       AND c.contype = 'n' AND a.attname = 'updated_at';

    IF v_valid AND v_upd_valid THEN
        RETURN;                         -- both columns are NOT NULL, nothing to do
    END IF;

    IF v_con IS NULL THEN
        ALTER TABLE context_blocks
            ADD CONSTRAINT context_blocks_created_at_not_null NOT NULL created_at NOT VALID;
        v_pending := v_pending || 'context_blocks_created_at_not_null'::text;
    ELSIF NOT v_valid THEN
        v_pending := v_pending || v_con;
    END IF;

    IF cardinality(v_pending) > 0 THEN
        WITH fixed AS (
            UPDATE context_blocks
               SET created_at = COALESCE(uuid_extract_timestamp(id), updated_at, now())
             WHERE created_at IS NULL
            RETURNING id
        )
        SELECT count(*), (array_agg(id ORDER BY id))[1:20] INTO v_fixed, v_ids FROM fixed;
        IF v_fixed > 0 THEN
            RAISE NOTICE 'context_blocks: % row(s) had no created_at; set from the id''s uuidv7 timestamp, else updated_at, else now() (first ids: %)',
                v_fixed, v_ids;
        END IF;
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
        RAISE WARNING 'context_blocks.created_at carries a not-null constraint that is not validated (%) and updated_at has % at % rows: every new row is checked for created_at and no row is without it. Add the constraint on updated_at and validate both out of band (runbook in the header of 186_blocks_created_at_not_null.sql); the schema-contract manifest reports updated_at until then.',
            COALESCE(NULLIF(array_to_string(v_pending, ', '), ''), 'none pending'),
            COALESCE(v_upd_con || ' (not validated)', 'none'), v_rows;
        RETURN;
    END IF;

    IF v_upd_con IS NULL THEN
        ALTER TABLE context_blocks
            ADD CONSTRAINT context_blocks_updated_at_not_null NOT NULL updated_at NOT VALID;
        v_pending := v_pending || 'context_blocks_updated_at_not_null'::text;
    ELSIF NOT v_upd_valid THEN
        v_pending := v_pending || v_upd_con;
    END IF;

    -- One ALTER TABLE validates both: one scan of the heap.
    BEGIN
        EXECUTE 'ALTER TABLE context_blocks '
             || (SELECT string_agg(format('VALIDATE CONSTRAINT %I', c), ', ') FROM unnest(v_pending) AS c);
    EXCEPTION WHEN not_null_violation THEN
        -- A row without updated_at: created_at is repaired above, and the
        -- NOT VALID constraints keep new rows out under this lock.
        WITH fixed AS (
            UPDATE context_blocks
               SET updated_at = COALESCE(created_at, uuid_extract_timestamp(id), now())
             WHERE updated_at IS NULL
            RETURNING id
        )
        SELECT count(*), (array_agg(id ORDER BY id))[1:20] INTO v_fixed, v_ids FROM fixed;
        RAISE NOTICE 'context_blocks: % row(s) had no updated_at; set from created_at, else the id''s uuidv7 timestamp, else now() (first ids: %)',
            v_fixed, v_ids;
        EXECUTE 'ALTER TABLE context_blocks '
             || (SELECT string_agg(format('VALIDATE CONSTRAINT %I', c), ', ') FROM unnest(v_pending) AS c);
    END;
END
$do$;
