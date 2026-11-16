-- =============================================================================
-- 175_embed_excluded.sql — a block of a type that is never embedded leaves
-- the embed-backfill pick indexes (issue #291, part 1)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- A block of a type whose retrieval policy is `excluded` (checkpoint and
-- system-meta on the shipped registry) is never embedded, so it stayed
-- pending for good: it sat in idx_embedding_pending_scope_sens and
-- idx_embedding_pending_sens (migration 171), and every pick of its class
-- walked past it and filtered it by the type predicate row by row. The cost
-- grew with the number of such rows (issue #291 measured, on the generic
-- plan, 517 -> 3 706 buffers for the caller's bound pick and 42 020 ->
-- 418 895 for the unbound old-end pick at 10 000 -> 100 000 checkpoint rows),
-- on every query that found nothing embeddable pending.
--
-- An index predicate cannot read context_block_types, so this migration
-- puts the registry's answer on the block:
--
--   context_blocks.embed_excluded  BOOLEAN NOT NULL DEFAULT false
--
-- true: the block's type is never embedded. The rule is one SQL function,
-- ctx_block_type_never_embedded(name), the rule of the embed pick before this
-- migration (store.RetrievalExcludedTypePredicate): the name's _global row
-- carries retrieval.policy `excluded`, and no row of the name in any other
-- scope carries another policy. The embed backfill has no reader identity,
-- so the second half is the union over all readers: an overlay that lifts
-- the exclusion keeps the type embeddable for everyone.
--
-- The answer depends on the _global row and on overlays that lift it, never
-- on a tenant's own registry writes: the narrowing gate of the write path
-- (blocktype.NarrowingViolation, axis N8 retrieval.policy, on create and
-- update of a non-_global row through every transport) lets an overlay of a
-- _global-excluded name carry `excluded` only, and a name without a _global
-- row is never marked. So no tenant can make the reconcile write blocks of
-- another tenant by changing its own types, and no tenant learns from its
-- own blocks whether another tenant holds a row of the same name. A type
-- that exists only in tenant scopes, excluded there, is filtered by the
-- pick row by row as before this migration (issue #213 stays open: marking
-- it would need the rule bound to the tenant that owns the block's scope,
-- like trg_block_retype in migration 174, and a queue per name and tenant).
--
-- WHO WRITES IT: only the trigger trg_block_embed_excluded, BEFORE INSERT OR
-- UPDATE OF type_name, embed_excluded, is_archived, FOR EACH ROW: an insert,
-- a re-type and an unarchive set the marker from the registry; a statement
-- that names the column gets the registry's answer, not its own; an archive
-- returns at once. When the answer is true the trigger decides again on a
-- fresh snapshot after it holds the name's lock, see LOCKS. The column list
-- names no embedding column on purpose: a column trigger binds to the
-- column's attnum, so after a re-embed cutover it would follow embedding_old
-- and block the cleanup's DROP COLUMN (2BP01). A block of a type that became
-- never-embedded while it held a vector keeps false until it is re-typed or
-- unarchived; when its vector is cleared it is filtered by the type
-- predicate as before this migration.
--
-- A REGISTRY CHANGE can turn the answer for a name: a write of its _global
-- row (a migration, an operator), or an overlay that lifts a _global
-- exclusion (written past the gate: psql, or a row that predates its _global
-- base). trg_block_types_embed_excluded, AFTER INSERT OR UPDATE OF name,
-- scope, config OR DELETE, FOR EACH ROW, queues the name in
-- context_embed_excluded_reconcile exactly then: the changed row is a _global
-- row that is or was excluded, or a non-_global row that is or was not
-- excluded while the name's _global row is excluded. A tenant overlay that
-- carries `excluded` queues nothing. The scheduler's reconcile
-- (events.runEmbedExcludedReconcile, store.ReconcileEmbedExcluded) sets the
-- marker of the queued names' live blocks in batches, outside the registry
-- write. A policy change of a type with millions of blocks
-- therefore costs the type API one queue row, not millions of row updates
-- in its request. Until the reconcile has passed:
--   * a name that became never-embedded: its unmarked pending blocks stay in
--     the pick indexes and are filtered by the type predicate as before this
--     migration (cost, not correctness);
--   * a name that became embeddable: its marked blocks are not picked until
--     the reconcile clears them (the embedding of such a type's blocks is
--     the long part either way).
-- This migration queues every name that is never embedded today, so the
-- reconcile marks the blocks that exist before it in the background: no row
-- of context_blocks is updated here.
--
-- INDEXES. The two pick indexes of migration 171 gain `AND NOT
-- embed_excluded` (same names, same keys); the pick states the conjunct
-- (store.PendingPickSQL), and with the old predicate it stays correct and
-- filters as before. idx_blocks_embed_excluded (type_name, id) WHERE
-- embed_excluded AND NOT is_archived is the reconcile's range for a name
-- whose marked blocks have to be cleared. It does not name the embedding
-- column, so it is not in the attnum class of the re-embed cutover; the
-- pick indexes are, and their _next twins (events.verifySisterIndexes) carry
-- the new conjunct.
--
-- INDEX BUILD AND ITS RUNBOOK (the 171 pattern). CREATE INDEX without
-- CONCURRENTLY takes a SHARE lock on context_blocks for the build: readers go
-- on, writers wait, and a partial index still reads the whole table. The
-- runner runs each file in ONE transaction, where CONCURRENTLY is impossible.
-- Per index:
--   * A usable index of this name with exactly the new definition: no-op.
--   * Below 500 000 rows (pg_class.reltuples) the migration builds inline
--     (a pick index of 171's definition is dropped first).
--   * Above, or a name taken by anything else: WARNING, nothing built or
--     dropped. Build out of band, on the running binary, BEFORE this binary
--     serves such a corpus (the column must exist first):
--
--       SET lock_timeout = '3s';
--       ALTER TABLE context_blocks
--           ADD COLUMN IF NOT EXISTS embed_excluded boolean NOT NULL DEFAULT false;
--       CREATE INDEX CONCURRENTLY idx_embedding_pending_scope_sens_291
--           ON context_blocks (scope, sensitivity, created_at, id)
--           WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded;
--       CREATE INDEX CONCURRENTLY idx_embedding_pending_sens_291
--           ON context_blocks (sensitivity, created_at, id)
--           WHERE embedding IS NULL AND NOT is_archived AND NOT embed_excluded;
--       CREATE INDEX CONCURRENTLY idx_blocks_embed_excluded
--           ON context_blocks (type_name, id)
--           WHERE embed_excluded AND NOT is_archived;
--       DROP INDEX CONCURRENTLY idx_embedding_pending_scope_sens;
--       ALTER INDEX idx_embedding_pending_scope_sens_291 RENAME TO idx_embedding_pending_scope_sens;
--       DROP INDEX CONCURRENTLY idx_embedding_pending_sens;
--       ALTER INDEX idx_embedding_pending_sens_291 RENAME TO idx_embedding_pending_sens;
--
--     (abortable; an aborted build leaves an INVALID index, which DROP INDEX
--     removes before the next try). Without the new pick indexes the picks
--     stay correct at the cost before this migration; without
--     idx_blocks_embed_excluded the reconcile of a name that became
--     embeddable walks idx_blocks_type_name. The schema-contract manifest
--     stays red until the build.
--
-- LOCKS. SET LOCAL lock_timeout = '3s' (house pattern 116/.../171/173/174):
-- ADD COLUMN with a constant default is a catalog change (ACCESS EXCLUSIVE
-- for the catalog write only, no rewrite); CREATE TRIGGER takes SHARE ROW
-- EXCLUSIVE; a transaction holding a table longer fails the migration with
-- 55P03, the file rolls back, the next boot tries again.
-- At run time the two triggers order themselves on one transaction-level
-- advisory lock per type name, hashtext('ctx:embed_excluded:' || name): a
-- block write whose answer is true takes it shared until its commit and then
-- decides again on a fresh snapshot; a registry change that queues the name
-- takes it exclusive before it queues. So a block written on the registry's
-- old state commits before the registry change does, and the reconcile,
-- which starts after it, sees it; a block written after waits and decides on
-- the new state. The lock is the only object both sides wait on (no tuple
-- lock on a registry row, so no lock-order cycle over a name's several rows
-- and no MultiXact on a hot builtin row); a registry change never waits on a
-- block row, and the reconcile holds no queue row while it updates blocks.
--
-- REVERT (forward-only tree; to undo temporarily): the column cannot be
-- dropped while a binary of this change runs. The triggers can:
--     DROP TRIGGER IF EXISTS trg_block_types_embed_excluded ON context_block_types;
--     DROP TRIGGER IF EXISTS trg_block_embed_excluded ON context_blocks;
-- (markers then stay as they are; the pick's type predicate keeps a block of
-- a type that became embeddable pickable only while its marker is false).
--
-- One column on context_blocks and one table: the test.sh T07 counts move to
-- 69 tables and 45 columns. The schema-contract manifest gains the column,
-- the table, the two functions, the two triggers, the reconcile index, the
-- two pick indexes' new definitions and migration_max 174 -> 175.
-- Idempotent, forward-only. Registration by the runner (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE context_blocks
    ADD COLUMN IF NOT EXISTS embed_excluded BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN context_blocks.embed_excluded IS
    'true: the block''s type is never embedded (ctx_block_type_never_embedded), so it is out of the embed-backfill pick indexes (migration 175). Written only by trg_block_embed_excluded.';

CREATE TABLE IF NOT EXISTS context_embed_excluded_reconcile (
    name      TEXT        PRIMARY KEY,
    queued_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

COMMENT ON TABLE context_embed_excluded_reconcile IS
    'Type names whose never-embedded answer may have changed; the scheduler sets the embed_excluded marker of their live blocks and removes the row (migration 175).';

CREATE OR REPLACE FUNCTION ctx_block_type_never_embedded(p_name TEXT) RETURNS BOOLEAN
LANGUAGE sql STABLE PARALLEL SAFE AS $$
    SELECT EXISTS (SELECT 1 FROM context_block_types t
                    WHERE t.name = p_name AND t.scope = '_global'
                      AND t.config->'retrieval'->>'policy' = 'excluded')
       AND NOT EXISTS (SELECT 1 FROM context_block_types t
                        WHERE t.name = p_name
                          AND t.config->'retrieval'->>'policy' IS DISTINCT FROM 'excluded')
$$;

COMMENT ON FUNCTION ctx_block_type_never_embedded(TEXT) IS
    'true when the _global row of the type name carries retrieval.policy excluded and no row of the name in another scope carries another policy: no reader ranks a vector of it, so the embed backfill skips it (migration 175, issue #291).';

CREATE OR REPLACE FUNCTION context_blocks_embed_excluded() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE'
       AND NEW.type_name IS NOT DISTINCT FROM OLD.type_name
       AND NEW.embed_excluded IS NOT DISTINCT FROM OLD.embed_excluded
       AND (NEW.is_archived OR NOT OLD.is_archived) THEN     -- not an unarchive
        RETURN NEW;
    END IF;
    NEW.embed_excluded := ctx_block_type_never_embedded(NEW.type_name);
    IF NEW.embed_excluded THEN
        -- Take the name's lock, then decide again on a fresh snapshot: a
        -- registry change that lifts the exclusion either waits for this
        -- commit (and its reconcile sees this row) or has committed before
        -- this second look.
        PERFORM pg_advisory_xact_lock_shared(hashtext('ctx:embed_excluded:' || NEW.type_name));
        NEW.embed_excluded := ctx_block_type_never_embedded(NEW.type_name);
    END IF;
    RETURN NEW;
END;
$$;

COMMENT ON FUNCTION context_blocks_embed_excluded() IS
    'trg_block_embed_excluded (migration 175): sets context_blocks.embed_excluded from the type registry on insert, re-type and unarchive.';

CREATE OR REPLACE TRIGGER trg_block_embed_excluded
    BEFORE INSERT OR UPDATE OF type_name, embed_excluded, is_archived ON context_blocks
    FOR EACH ROW EXECUTE FUNCTION context_blocks_embed_excluded();

CREATE OR REPLACE FUNCTION context_block_types_embed_excluded() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_name TEXT;
BEGIN
    IF TG_OP = 'UPDATE'
       AND NEW.name IS NOT DISTINCT FROM OLD.name
       AND NEW.scope IS NOT DISTINCT FROM OLD.scope
       AND (NEW.config->'retrieval'->>'policy') IS NOT DISTINCT FROM (OLD.config->'retrieval'->>'policy') THEN
        RETURN NULL;            -- the answer depends on (name, scope, policy) only
    END IF;
    FOR v_name IN
        SELECT DISTINCT n FROM unnest(ARRAY[
            CASE WHEN TG_OP <> 'INSERT' THEN OLD.name END,
            CASE WHEN TG_OP <> 'DELETE' THEN NEW.name END]) AS n
         WHERE n IS NOT NULL
    LOOP
        -- The answer for v_name can only have changed if this row is a
        -- _global row that is or was excluded, or a row of another scope that
        -- is or was not excluded while the _global row is excluded. A tenant
        -- overlay carrying excluded (the only one the narrowing gate admits
        -- for such a name) changes nothing and queues nothing.
        IF (TG_OP <> 'INSERT' AND OLD.name = v_name AND OLD.scope = '_global'
                AND OLD.config->'retrieval'->>'policy' = 'excluded')
           OR (TG_OP <> 'DELETE' AND NEW.name = v_name AND NEW.scope = '_global'
                AND NEW.config->'retrieval'->>'policy' = 'excluded')
           OR (((TG_OP <> 'INSERT' AND OLD.name = v_name AND OLD.scope <> '_global'
                    AND OLD.config->'retrieval'->>'policy' IS DISTINCT FROM 'excluded')
                OR (TG_OP <> 'DELETE' AND NEW.name = v_name AND NEW.scope <> '_global'
                    AND NEW.config->'retrieval'->>'policy' IS DISTINCT FROM 'excluded'))
               AND EXISTS (SELECT 1 FROM context_block_types t
                            WHERE t.name = v_name AND t.scope = '_global'
                              AND t.config->'retrieval'->>'policy' = 'excluded')) THEN
            PERFORM pg_advisory_xact_lock(hashtext('ctx:embed_excluded:' || v_name));
            INSERT INTO context_embed_excluded_reconcile (name) VALUES (v_name)
                ON CONFLICT (name) DO UPDATE SET queued_at = clock_timestamp();
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$;

COMMENT ON FUNCTION context_block_types_embed_excluded() IS
    'trg_block_types_embed_excluded (migration 175): queues a type name for the embed_excluded reconcile when a registry change may turn its never-embedded answer.';

CREATE OR REPLACE TRIGGER trg_block_types_embed_excluded
    AFTER INSERT OR UPDATE OF name, scope, config OR DELETE ON context_block_types
    FOR EACH ROW EXECUTE FUNCTION context_block_types_embed_excluded();

-- The names that are never embedded today: the reconcile marks their blocks.
INSERT INTO context_embed_excluded_reconcile (name)
SELECT DISTINCT t.name FROM context_block_types t
 WHERE ctx_block_type_never_embedded(t.name)
ON CONFLICT (name) DO NOTHING;

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
            ('idx_embedding_pending_scope_sens', 'scope, sensitivity, created_at, id',
             '((embedding IS NULL) AND (NOT is_archived) AND (NOT embed_excluded))',
             'embedding IS NULL AND NOT is_archived AND NOT embed_excluded',
             '((embedding IS NULL) AND (NOT is_archived))'),
            ('idx_embedding_pending_sens', 'sensitivity, created_at, id',
             '((embedding IS NULL) AND (NOT is_archived) AND (NOT embed_excluded))',
             'embedding IS NULL AND NOT is_archived AND NOT embed_excluded',
             '((embedding IS NULL) AND (NOT is_archived))'),
            ('idx_blocks_embed_excluded', 'type_name, id',
             '(embed_excluded AND (NOT is_archived))',
             'embed_excluded AND NOT is_archived',
             NULL)
        ) AS t(name, cols, pred_deparsed, pred, old_pred_deparsed)
    LOOP
        SELECT pg_get_indexdef(c.oid), i.indisvalid INTO v_def, v_valid
          FROM pg_class c
          JOIN pg_namespace n ON n.oid = c.relnamespace
          JOIN pg_index     i ON i.indexrelid = c.oid
         WHERE n.nspname = 'public' AND c.relname = v_idx.name;

        IF FOUND THEN
            IF v_valid AND v_def = format('CREATE INDEX %s ON public.context_blocks USING btree (%s) WHERE %s',
                                          v_idx.name, v_idx.cols, v_idx.pred_deparsed) THEN
                CONTINUE;               -- usable, nothing to do
            END IF;
            IF v_idx.old_pred_deparsed IS NOT NULL AND v_rows < 500000
               AND v_def = format('CREATE INDEX %s ON public.context_blocks USING btree (%s) WHERE %s',
                                  v_idx.name, v_idx.cols, v_idx.old_pred_deparsed) THEN
                EXECUTE format('DROP INDEX %I', v_idx.name);
            ELSE
                RAISE WARNING '% exists but is not the index of migration 175 (valid=%): %. The embed-backfill pick stays correct at its cost before 175. Runbook in the header of 175_embed_excluded.sql; the schema-contract manifest stays red until then.',
                    v_idx.name, v_valid, v_def;
                CONTINUE;
            END IF;
        END IF;

        IF v_rows < 500000 THEN
            EXECUTE format('CREATE INDEX IF NOT EXISTS %I ON context_blocks (%s) WHERE %s',
                           v_idx.name, v_idx.cols, v_idx.pred);
        ELSE
            RAISE WARNING '% is missing at % rows: build it out of band (CREATE INDEX CONCURRENTLY, runbook in the header of 175_embed_excluded.sql). The embed-backfill pick stays correct at its cost before 175; the schema-contract manifest stays red.',
                v_idx.name, v_rows;
        END IF;
    END LOOP;
END
$do$;
