-- =============================================================================
-- 171_embed_pending_class_indexes.sql — the embed-backfill pick reads the
-- pending rows of the classes it can return, not all of them (issue #239)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Both embed-backfill picks (the query path's, handler.backfillPicker, and the
-- background arm's, events.backfillStep) read idx_embedding_pending
-- (created_at) WHERE embedding IS NULL AND NOT is_archived and filtered scope
-- and class row by row. A block no embed backend may receive (its
-- floor-adjusted sensitivity is above the trust of every backend its scope can
-- use) gets no vector and no memo and stays pending, so every pick walked past
-- all of them; on the generic plan a driver-cached statement switches to after
-- its fifth execution, a pick bound to some scopes walked past the pending
-- rows of every other scope as well. Measured before this migration (generic
-- plan, the pins of the issue): the caller's pick 25 401 buffers at 10 000
-- unservable, floored-out and foreign pending rows each, 253 980 at 100 000.
--
-- Since this migration the pick is one LIMIT 1 branch per class
-- (store.PendingPickSQL) and each branch is one range of one of these two
-- partial indexes:
--   idx_embedding_pending_scope_sens (scope, sensitivity, created_at, id)
--     a pick bound to scopes (the caller's read scopes, a tenant's owned
--     scopes): one branch per scope and sensitivity it may return;
--   idx_embedding_pending_sens (sensitivity, created_at, id)
--     an unbound pick (the old end with the arm off, the arm's legacy
--     unbounded pass): one branch per sensitivity the shared backends take.
-- (created_at, id) closes both keys, so a branch is an index scan forward
-- (oldest first) or backward (newest first) that stops at its first row; the
-- picks order by created_at and id, and the incremental sort the
-- created_at-only key forced is gone. idx_embedding_pending stays: the
-- /status backlog guard, the re-embed migration's twins and renames and its
-- tests read it.
--
-- Both predicates are the backfill predicate of idx_embedding_pending and bind
-- to the physical embedding column, so both are in the attnum class of the
-- re-embed cutover: their _next twins are built by the verify gate
-- (events.verifySisterIndexes) and renamed by the swap
-- (events.cutoverIndexRenames) like the other three.
--
-- INDEX BUILD AND ITS RUNBOOK (115/140 pattern). CREATE INDEX without
-- CONCURRENTLY takes a SHARE lock on context_blocks for the build: readers go
-- on, writers wait. A partial index still reads the whole table, so the build
-- time follows the table, not the pending rows. The runner runs each file in
-- ONE transaction (store/migrations.go), where CONCURRENTLY is impossible.
-- Therefore, per index:
--   * A usable index of this name (btree, valid, on context_blocks, exactly
--     this key and predicate) makes this a no-op. An operator can build it
--     beforehand, out of band:
--
--       CREATE INDEX CONCURRENTLY idx_embedding_pending_scope_sens
--           ON context_blocks (scope, sensitivity, created_at, id)
--           WHERE embedding IS NULL AND NOT is_archived;
--       CREATE INDEX CONCURRENTLY idx_embedding_pending_sens
--           ON context_blocks (sensitivity, created_at, id)
--           WHERE embedding IS NULL AND NOT is_archived;
--
--     (abortable; an aborted build leaves an INVALID index behind, which is
--     dropped with DROP INDEX before the next try).
--   * A name taken by anything else: WARNING, nothing built.
--   * Below 500 000 rows (pg_class.reltuples) the migration builds inline.
--   * Above, WARNING, nothing built. The picks then run their branches on
--     idx_embedding_pending and the other indexes of context_blocks, which is
--     correct but not bounded; the schema-contract manifest stays red until
--     the out-of-band build (the deliberate trade of 115 and 140).
--
-- `SET LOCAL lock_timeout = '3s'` (house pattern 116/117/126/128/140/145):
-- the 3 s bound acquiring the lock, not the build. A writer holding it longer
-- fails the migration with lock_not_available (55P03), the file rolls back
-- and the boot fails loudly; the next boot tries again.
--
-- REVERT (forward-only tree; to undo temporarily):
--     DROP INDEX IF EXISTS idx_embedding_pending_scope_sens;
--     DROP INDEX IF EXISTS idx_embedding_pending_sens;
-- The picks stay correct without them (previous paragraph).
--
-- Index change only, no table or column: the test.sh T07 counts stay. The
-- schema-contract manifest gains the two indexes and migration_max 170 → 171.
-- Idempotent, forward-only. Registration by the runner (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

DO $do$
DECLARE
    v_idx   RECORD;
    v_def   TEXT;
    v_valid BOOLEAN;
    v_rows  BIGINT;
BEGIN
    SELECT c.reltuples::BIGINT INTO v_rows
      FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'public' AND c.relname = 'context_blocks';
    IF v_rows IS NULL OR v_rows < 0 THEN
        -- reltuples < 0: never analysed. A bounded count, never a full scan.
        SELECT count(*) INTO v_rows FROM (SELECT 1 FROM context_blocks LIMIT 500000) t;
    END IF;

    FOR v_idx IN
        SELECT * FROM (VALUES
            ('idx_embedding_pending_scope_sens', 'scope, sensitivity, created_at, id'),
            ('idx_embedding_pending_sens',       'sensitivity, created_at, id')
        ) AS t(name, cols)
    LOOP
        SELECT pg_get_indexdef(c.oid), i.indisvalid INTO v_def, v_valid
          FROM pg_class c
          JOIN pg_namespace n ON n.oid = c.relnamespace
          JOIN pg_index     i ON i.indexrelid = c.oid
         WHERE n.nspname = 'public' AND c.relname = v_idx.name;

        IF FOUND THEN
            -- The definition as PostgreSQL deparses it, so a same-named index
            -- on another table, column list or predicate is not adopted.
            IF v_valid AND v_def = format(
                'CREATE INDEX %s ON public.context_blocks USING btree (%s) WHERE ((embedding IS NULL) AND (NOT is_archived))',
                v_idx.name, v_idx.cols)
            THEN
                CONTINUE;               -- usable, nothing to do
            END IF;
            RAISE WARNING '% exists but is not the embed-backfill pick index (valid=%): %. The pick stays correct and unbounded. Runbook in the header of 171_embed_pending_class_indexes.sql: rename or drop it, then build it CONCURRENTLY; the schema-contract manifest stays red until then.',
                v_idx.name, v_valid, v_def;
            CONTINUE;
        END IF;

        IF v_rows < 500000 THEN
            EXECUTE format(
                'CREATE INDEX IF NOT EXISTS %I ON context_blocks (%s) WHERE embedding IS NULL AND NOT is_archived',
                v_idx.name, v_idx.cols);
        ELSE
            RAISE WARNING '% is missing at % rows: build it out of band (CREATE INDEX CONCURRENTLY, runbook in the header of 171_embed_pending_class_indexes.sql). The embed-backfill pick stays correct and unbounded until then; the schema-contract manifest stays red.',
                v_idx.name, v_rows;
        END IF;
    END LOOP;
END
$do$;
