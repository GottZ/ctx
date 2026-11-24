-- =============================================================================
-- 193_embed_pending_type_index.sql — the embed-excluded mark reads the
-- pending blocks of its name and scope, not every pending block of the scope
-- (issue #527)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- The reconcile of the embed_excluded marker (store.ReconcileEmbedExcluded,
-- migrations 175 and 176) marks, for a type name and a scope whose answer is
-- "never embedded", the scope's unmarked blocks of the name that wait for a
-- vector, and then checks from the start whether one is left. The only
-- bounded range for that was the scope's range of
-- idx_embedding_pending_scope_sens (scope, sensitivity, created_at, id)
-- WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded, which
-- holds the scope's pending blocks of every name: each batch statement and
-- each left check read all of them and checked the name on the heap. A scope
-- whose sensitivity floor keeps its blocks above every backend it may use
-- stays pending for good, and any scope does during a long embedding
-- backlog. Measured on the production and the CI image (docs/operations.md,
-- "Migration 193"), a scope with 2 500 blocks of the name beside 10 000 /
-- 100 000 / 1 000 000 pending blocks of another type, once its blocks of
-- the name are marked (what every later batch and left check reads), at
-- random_page_cost 1.1 and 4, custom and generic plan: 1 056 / 10 394 /
-- 103 766 buffers per batch and 1 058 / 10 396 / 103 768 per left check,
-- 0.5-1.6 s per statement at 1 000 000.
--
-- This migration adds:
--
--   idx_embedding_pending_type_scope ON context_blocks (type_name, scope, id)
--       WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded
--
-- The predicate is that of the two backfill pick indexes (171, predicate
-- since 175): a block enters while it waits for a vector and is not marked.
-- The key is what the mark asks for: the name and the scope as the leading
-- range, then id as the keyset of its batches (store.markEmbedExcludedSQL
-- reads ($1, $2, cursor) < (type_name, scope, id) <= ($1, $2) in key order,
-- the form of the clear on idx_blocks_embed_excluded (type_name, scope, id)
-- of 176), and the left check reads the first entry of the same range. The
-- name leads, not the scope: the '*' entry of a _global change lists the
-- scopes of the name's pending blocks (store.embedExcludedStarScopesSQL),
-- which is the name's range of this index; with the scope leading it would
-- be no range of it. The mark runs with sorts off, where this range is the
-- only path without a sort. Measured on the same stores: 18-19 buffers per
-- batch and 20-21 per left check at each of the three sizes (the entries
-- the marked rows leave until a vacuum; after one, 4-6).
--
-- Write cost: a block enters the index on its insert (every block is written
-- without a vector and gets it from the backfill) and on every update that
-- is not HOT while it waits; the update that stores its vector writes no
-- entry (the new row version is outside the predicate), and the old entry
-- goes with vacuum or the index's own cleanup of dead entries. type_name,
-- scope, id, embedding, is_archived and embed_excluded are indexed already
-- (idx_blocks_type_name, idx_context_scope, the primary key, the pick
-- indexes), so the index takes no update off the HOT path. Measured at
-- 300 000 rows, 285 000 of them pending: 2 000 writes per round showed no
-- difference with and without the index (5 761-7 490 bytes of WAL and
-- 1.9-2.6 ms per block written without a vector, 9 887-9 969 bytes and
-- 0.67-0.75 ms per vector stored). Size: 16.9 MB built at 285 000 entries
-- (about 59 bytes each); 95 MB at 950 000 entries grown by inserts, as
-- idx_embedding_pending_sens beside it.
--
-- WHY THE STATEMENTS BEFORE 193 STAY. Without this index (below), the mark's
-- selection on the name and the scope has no range: every other path is the
-- name's range in every tenant (idx_blocks_type_name), the scope's whole
-- range (idx_context_scope) or the pending blocks of the whole table. So the
-- reconcile of this release reads the catalog once per pass with a queued
-- entry (store.embedExcludedPendingIndexReadySQL: valid, ready, exactly this
-- definition) and, while the index does not stand so, sends the mark and its
-- left check of the release before 193, at their cost.
--
-- RE-EMBED. The predicate names the embedding column, so the index is in the
-- attnum class of the re-embed cutover: its twin
-- idx_embedding_pending_type_scope_next (events.verifySisterIndexes) carries
-- this definition on embedding_next and the swap renames it onto this name,
-- and this name onto idx_embedding_pending_type_scope_old. A migration that
-- reached verifying on an older release has no such twin: the verify gate
-- builds it, and on a green verdict the migration cycle builds it alone
-- (issue #333); the confirm refuses until it stands. A swap by a binary
-- before 193 renames nothing of this index: it then follows embedding_old
-- (the readiness check above no longer matches, the reconcile sends the
-- statements before 193) and goes with that column at the cleanup; a
-- rollback of such a swap by this release leaves it under this name, on
-- embedding again.
--
-- INDEX BUILD AND ITS RUNBOOK (docs/development.md, "A migration with a row
-- threshold"). The runner runs each file in ONE transaction
-- (store/migrations.go), where CONCURRENTLY is impossible. CREATE INDEX takes
-- SHARE on context_blocks until the file commits: readers go on, every
-- INSERT, UPDATE and DELETE waits; the build reads the whole heap, so its
-- time follows the table, not the index.
--   * A valid index of this name with exactly this definition, as
--     PostgreSQL deparses it:
--       CREATE INDEX idx_embedding_pending_type_scope ON public.context_blocks USING btree (type_name, scope, id) WHERE ((embedding IS NULL) AND (NOT is_archived) AND (NOT embed_excluded))
--     no-op.
--   * Below 500 000 rows (ctx_migration_rows, migration 191) and nothing of
--     this name: built inline. Measured with a writer inserting a block
--     every 2 ms (rows of about 1.5 KB of heap, 19 of 20 pending, host
--     load 6-12): the build and the writer's longest wait 0.60-0.66 s at
--     250 000 rows, 1.38-1.87 s at 500 000; through the runner at 450 000
--     rows (host load 27) 2.1 s, the writer waiting 2.0 s; at 1 000 000
--     rows 2.2-3.6 s.
--   * Above, or a name taken by anything else (another definition, an
--     INVALID index — an aborted CONCURRENTLY build — or a relation that is
--     not an index): WARNING, nothing built or dropped, and no SHARE lock on
--     context_blocks (measured through the runner at 1 000 000 rows: 36 ms,
--     no lock on context_blocks a 2 ms sampler saw). Build it out of band
--     on the running binary
--     (readers and writers go on). First, what holds the name:
--
--       SELECT c.relkind, i.indisvalid, pg_get_indexdef(c.oid)
--         FROM pg_class c
--         JOIN pg_namespace n ON n.oid = c.relnamespace
--         LEFT JOIN pg_index i ON i.indexrelid = c.oid
--        WHERE n.nspname = 'public' AND c.relname = 'idx_embedding_pending_type_scope';
--
--     No row: build it. An index, valid, of the definition above: keep it,
--     nothing to build. That definition with indisvalid false (an aborted
--     build): DROP INDEX CONCURRENTLY idx_embedding_pending_type_scope; then
--     build it. Anything else (another definition, a view, a table, a
--     sequence: not ctx's to drop): rename it, then build it. The build:
--
--       CREATE INDEX CONCURRENTLY idx_embedding_pending_type_scope
--           ON context_blocks (type_name, scope, id)
--           WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded;
--
--     No IF NOT EXISTS: it would pass over an INVALID index or a foreign
--     object of the name. Until the index stands with this definition the
--     reconcile sends the statements before 193, at their cost, and the
--     schema-contract manifest stays red. Measured at 1 000 000 rows:
--     2.9-3.9 s, a writer's longest wait 28-256 ms.
--
-- Rows, not the heap: the threshold counts live rows, the build reads every
-- heap page. A context_blocks heap far larger than its rows (mass deletes)
-- passes as small and makes the inline build read its size: compact it out
-- of band first, or build the index by the runbook before the upgrade.
--
-- `SET LOCAL lock_timeout = '3s'` (house pattern) bounds the acquisition of
-- the SHARE lock, not the build: a transaction that holds a write on
-- context_blocks longer fails the migration with lock_not_available (55P03),
-- the file rolls back with nothing changed and the boot fails loudly; the
-- next boot tries again. ctx_migration_rows takes ACCESS SHARE on
-- context_blocks for its measure and releases it, and its count, where it
-- counts, holds ACCESS SHARE to the commit; writers wait for neither.
--
-- OLDER BINARY. v5.78.0 (the rollback image) marks with its statements, the
-- ones this release sends before 193; the index costs it the write of its
-- entries and nothing else. Its migration runner leaves version 193 alone.
-- Its schema-contract check finds _migrations:193 without an embedded file
-- (breaking) and an index it does not know: in mode warn (the default) it
-- starts and /health reports schema_contract attention; in mode enforce it
-- refuses to start until CTX_CONTRACT_MODE=warn or off is set. Its re-embed
-- cutover renames nothing of this index (RE-EMBED above), its rollback
-- neither: a swap of this release rolled back by it leaves the twin under
-- this name on embedding_next and the old index as _old on embedding, so
-- roll back a swap of this release with this release.
--
-- WAY BACK: only when the check query above shows an index of the definition
-- above (any validity) — a same-named object of another kind or definition
-- is one this file left alone with a WARNING, and it stays —
-- DROP INDEX CONCURRENTLY idx_embedding_pending_type_scope; then
-- DELETE FROM _migrations WHERE version = 193. The reconcile of this release
-- sends the statements before 193 from its next pass on.
--
-- Index change only, no table or column: the test.sh T07 counts stay. The
-- schema-contract manifest gains the index and migration_max 192 -> 193.
-- Idempotent, forward-only. Registration by the runner (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

DO $do$
DECLARE
    -- Measured: the build at the threshold and what waits for it
    -- (docs/operations.md, "Migration 193").
    inline_rows CONSTANT BIGINT := 500000;
    c_def       CONSTANT TEXT := 'CREATE INDEX idx_embedding_pending_type_scope ON public.context_blocks USING btree (type_name, scope, id) WHERE ((embedding IS NULL) AND (NOT is_archived) AND (NOT embed_excluded))';
    v_kind      TEXT;
    v_def       TEXT;
    v_valid     BOOLEAN;
    v_rows      BIGINT;
BEGIN
    -- Whatever relation holds the name: CREATE INDEX could not pass a view,
    -- a table or a sequence of this name either.
    SELECT c.relkind::text, pg_get_indexdef(i.indexrelid), i.indisvalid INTO v_kind, v_def, v_valid
      FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
      LEFT JOIN pg_index i ON i.indexrelid = c.oid
     WHERE n.nspname = 'public' AND c.relname = 'idx_embedding_pending_type_scope';

    IF FOUND THEN
        -- The definition as PostgreSQL deparses it, so a same-named index on
        -- another table, key or predicate is not adopted.
        IF v_kind = 'i' AND v_valid AND v_def = c_def THEN
            RETURN;                 -- usable, nothing to do
        END IF;
        RAISE WARNING 'idx_embedding_pending_type_scope exists but is not the index of migration 193 (valid=%): %. Nothing built or dropped; until it is replaced the embed-excluded reconcile marks with its statements before 193, at their cost before 193. Runbook in the header of 193_embed_pending_type_index.sql; the schema-contract manifest stays red until then.',
            COALESCE(v_valid, false), COALESCE(v_def, format('a relation of relkind %s, not an index', v_kind));
        RETURN;
    END IF;

    v_rows := ctx_migration_rows('public.context_blocks', inline_rows);
    IF v_rows >= inline_rows THEN
        RAISE WARNING 'idx_embedding_pending_type_scope is missing at % rows: build it out of band (CREATE INDEX CONCURRENTLY, runbook in the header of 193_embed_pending_type_index.sql). Until then the embed-excluded reconcile marks with its statements before 193, at their cost before 193; the schema-contract manifest stays red.',
            v_rows;
        RETURN;
    END IF;

    CREATE INDEX idx_embedding_pending_type_scope
        ON context_blocks (type_name, scope, id)
        WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded;
END
$do$;
