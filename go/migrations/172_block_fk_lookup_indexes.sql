-- =============================================================================
-- 172_block_fk_lookup_indexes.sql — the last two foreign keys on
-- context_blocks without a serving index get one (issue #255)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Deleting a block runs one lookup per foreign key that points at
-- context_blocks(id), on the referencing table, `WHERE col = $1`: CASCADE
-- deletes the rows found, SET NULL updates them, NO ACTION checks for them.
-- Of the 14 such keys two had no index whose leading column is the key, so
-- their lookup was a sequential scan of the referencing table, once per
-- deleted block:
--   context_write_log.matched_block_id  ON DELETE SET NULL  (the guard batch
--     writes one row per block it checks and sets it to the nearest candidate)
--   context_embed_failures.block_id     ON DELETE CASCADE   (its two unique
--     indexes are partial on migration_id IS NULL / IS NOT NULL, and
--     `block_id = $1` implies neither)
-- A purge batch, the tenant prune and the parent_id cascade all pay it.
-- Measured for the issue (scope purge, 10 000 blocks): 29 ms per block next
-- to a 300 000-row write log, 0.12 ms with the write-log index. Measured
-- for this migration (testcontainer, 200 deleted blocks): write log 0.74 /
-- 6.0 / 17.6 ms per block at 10 000 / 100 000 / 300 000 rows, 0.19 / 0.12 /
-- 0.12 ms with the index; embed failures 0.61 / 5.2 ms per block at 10 000 /
-- 100 000 rows, 0.08 ms with the index.
--
--   idx_write_log_matched_block (matched_block_id) WHERE matched_block_id IS NOT NULL
--     partial: the lookup's `matched_block_id = $1` implies the predicate,
--     and the rows without a match (dream audit rows, guard decisions
--     without a candidate) stay out of it.
--   idx_embed_failures_block (block_id)
--     block_id is NOT NULL, so the plain index is the partial one.
--
-- The catalog gate store.TestBlockForeignKeysHaveAServingIndex holds every
-- key on context_blocks to this: a new key without a serving index fails it.
-- Neither table is a hypertable (TimescaleDB copies a key on a hypertable
-- onto every chunk, and a delete probes each copy; the gate refuses that).
--
-- INDEX BUILD AND ITS RUNBOOK (115/140/171 pattern). CREATE INDEX without
-- CONCURRENTLY takes a SHARE lock on the indexed table for the build:
-- readers go on, writers wait. ctxd runs its migrations before it starts the
-- scheduler and the router (cmd/ctxd/main.go), so on the booting instance
-- nothing waits but the boot itself; on other instances of the same database:
--   context_write_log      the guard batch (guard.writeAuditLog, a scheduler
--                          job), the dream audit rows (dream.WriteLinks,
--                          store.DreamLinkResolve) and every block delete
--                          (the SET NULL of both write-log keys);
--   context_embed_failures every content change of a block
--                          (store.ClearEmbeddingTx drops its backfill memo in
--                          the writer's transaction), the backfill's failure
--                          memos and every block delete (CASCADE).
-- No client store writes the write log. The runner runs each file in ONE
-- transaction (store/migrations.go), where CONCURRENTLY is impossible.
-- Therefore, per index:
--   * A usable index of this name (btree, valid, on its table, exactly this
--     key and predicate) makes this a no-op. An operator can build it
--     beforehand, out of band:
--
--       CREATE INDEX CONCURRENTLY idx_write_log_matched_block
--           ON context_write_log (matched_block_id)
--           WHERE matched_block_id IS NOT NULL;
--       CREATE INDEX CONCURRENTLY idx_embed_failures_block
--           ON context_embed_failures (block_id);
--
--     (abortable; an aborted build leaves an INVALID index behind, which is
--     dropped with DROP INDEX before the next try).
--   * A name taken by anything else: WARNING, nothing built.
--   * Below the row threshold of its table (pg_class.reltuples) the migration
--     builds inline:
--       context_write_log       3 000 000 rows. Only background jobs and
--         deletes wait, so the bound is the boot: measured with the guard's
--         row width and distinct keys, 0.12 s at 500 000 rows, 0.24 s at
--         1 000 000, 1.1 s at 3 000 000, 3.7 s at 5 000 000 (heap 1.3 GB),
--         and an insert issued during the build waited for all of it.
--       context_embed_failures    500 000 rows (the house value of 115, 140
--         and 171): content changes wait for it. The table holds at most one
--         row per block and re-embed migration that failed; 23 ms at 100 000.
--   * Above, WARNING, nothing built. Build it out of band: until then every
--     deleted block scans that table once, as before this migration, and the
--     schema-contract manifest stays red (the deliberate trade of 115, 140
--     and 171): /health reports degraded in contract mode warn, and a boot in
--     contract mode enforce refuses to start.
-- The write log goes first: its lock is held to the commit, so it then also
-- covers the short embed-failure build, and the lock content changes wait for
-- is taken last and held for its own build alone. A block delete on another
-- instance locks both tables too, in the order its foreign-key triggers fire;
-- should that close a cycle with the migration, PostgreSQL aborts one of the
-- two (40P01): a purge batch repeats, a boot fails and the next one retries
-- (a reading of the lock order, not reproduced).
--
-- `SET LOCAL lock_timeout = '3s'` (house pattern 116/117/126/128/140/145/171):
-- the 3 s bound acquiring each lock, not the build. A writer holding it longer
-- fails the migration with lock_not_available (55P03), the file rolls back
-- and the boot fails loudly; the next boot tries again. While the migration
-- waits for a lock, the writers of that table queue behind it, for at most
-- those 3 s.
--
-- REVERT (forward-only tree; to undo temporarily):
--     DROP INDEX IF EXISTS idx_write_log_matched_block;
--     DROP INDEX IF EXISTS idx_embed_failures_block;
-- Deletes stay correct without them, at the cost named above.
--
-- Index change only, no table or column: the test.sh T07 counts stay. The
-- schema-contract manifest gains the two indexes and migration_max 171 → 172.
-- Idempotent, forward-only. Registration by the runner (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

DO $do$
DECLARE
    v_idx   RECORD;
    v_def   TEXT;
    v_valid BOOLEAN;
    v_rows  BIGINT;
    v_want  TEXT;
BEGIN
    FOR v_idx IN
        SELECT * FROM (VALUES
            ('idx_write_log_matched_block', 'context_write_log',      'matched_block_id', 'matched_block_id IS NOT NULL', 3000000),
            ('idx_embed_failures_block',    'context_embed_failures', 'block_id',         NULL,                            500000)
        ) AS t(name, tbl, col, pred, max_rows)
    LOOP
        -- The definition as PostgreSQL deparses it, so a same-named index
        -- on another table, column or predicate is not adopted.
        v_want := format('CREATE INDEX %s ON public.%s USING btree (%s)', v_idx.name, v_idx.tbl, v_idx.col)
               || CASE WHEN v_idx.pred IS NULL THEN '' ELSE format(' WHERE (%s)', v_idx.pred) END;

        SELECT pg_get_indexdef(c.oid), i.indisvalid INTO v_def, v_valid
          FROM pg_class c
          JOIN pg_namespace n ON n.oid = c.relnamespace
          JOIN pg_index     i ON i.indexrelid = c.oid
         WHERE n.nspname = 'public' AND c.relname = v_idx.name;

        IF FOUND THEN
            IF v_valid AND v_def = v_want THEN
                CONTINUE;               -- usable, nothing to do
            END IF;
            RAISE WARNING '% exists but is not the foreign-key lookup index of %(%) (valid=%): %. Deleting a block scans % once per block until it is there. Runbook in the header of 172_block_fk_lookup_indexes.sql: rename or drop it, then build it CONCURRENTLY; the schema-contract manifest stays red until then.',
                v_idx.name, v_idx.tbl, v_idx.col, v_valid, v_def, v_idx.tbl;
            CONTINUE;
        END IF;

        SELECT c.reltuples::BIGINT INTO v_rows
          FROM pg_class c
          JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'public' AND c.relname = v_idx.tbl;
        IF v_rows IS NULL OR v_rows < 0 THEN
            -- reltuples < 0: never analysed. A bounded count, never a full scan.
            EXECUTE format('SELECT count(*) FROM (SELECT 1 FROM %I LIMIT %s) t', v_idx.tbl, v_idx.max_rows) INTO v_rows;
        END IF;

        IF v_rows < v_idx.max_rows THEN
            EXECUTE format('CREATE INDEX IF NOT EXISTS %I ON %I (%I)', v_idx.name, v_idx.tbl, v_idx.col)
                 || CASE WHEN v_idx.pred IS NULL THEN '' ELSE ' WHERE ' || v_idx.pred END;
        ELSE
            RAISE WARNING '% is missing at % rows of %: build it out of band (CREATE INDEX CONCURRENTLY, runbook in the header of 172_block_fk_lookup_indexes.sql). Deleting a block scans % once per block until then; the schema-contract manifest stays red.',
                v_idx.name, v_rows, v_idx.tbl, v_idx.tbl;
        END IF;
    END LOOP;
END
$do$;
