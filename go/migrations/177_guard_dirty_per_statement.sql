-- =============================================================================
-- 177_guard_dirty_per_statement.sql — the guard's dirty marker is written once
-- per statement instead of once per row (issue #336)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- trg_guard_dirty was AFTER INSERT OR UPDATE … FOR EACH ROW: for every inserted
-- or updated block row, mark_guard_dirty() ran
--   UPDATE context_guard_state SET dirty_since = COALESCE(dirty_since, now()),
--                                  pending_count = pending_count + 1
-- on the one state row. Every such update leaves a version of that row which
-- the writing transaction cannot prune before it commits, and the next update
-- of the same transaction reads past all of them (testcontainer: after a
-- 20 000-row insert in one transaction the state table had grown from 8 KB to
-- 1 MB and one more update of the row read 130 buffers; with this migration
-- 8 KB and 3 buffers). The cost per row grew with the rows of the statement
-- (issue #336: 247 µs per inserted row in statements of 5 000-10 000 rows,
-- 837 µs at 45 000-90 000 rows, 149-155 µs at any size without the trigger).
--
-- Since this migration the trigger is statement-level with a transition table,
-- and a second one covers UPDATE (PostgreSQL allows one event per trigger with
-- transition tables, house precedent 170): mark_guard_dirty() counts the
-- statement's qualifying rows and adds the count in ONE update of the state
-- row. A row qualifies exactly when the row-level function counted it:
--   INSERT  category is not 'index'.
--   UPDATE  the new row's category is not 'index', and not (the old and the
--           new metadata both carry the key guard_status and content is
--           unchanged) — the guard's verdict write, and any other update of a
--           checked block that leaves its content alone (tags, embedding,
--           lifecycle).
-- A skip condition that is NULL does not skip, as in the plpgsql IF before
-- (NOT EXISTS keeps only TRUE, IS DISTINCT FROM counts a NULL category). The
-- old row of an UPDATE is found by id. Two declared differences, neither on a
-- code path; the counter is read for display only (manage guard-stats):
--   - An UPDATE that changes a block's id finds no old row under the new id
--     (or the old row of another block that had it): the row counts unless
--     that row satisfies the skip rule. The row-level trigger compared OLD and
--     NEW of the same row.
--   - A statement that updates the same block twice at one query level (a
--     writable CTE plus a foreign-key action on that block) puts both versions
--     of the id into old_rows and new_rows. Each new version counts once, and
--     it is skipped when any old version of the id satisfies the skip rule;
--     the row-level trigger compared each update with its own old version.
--
-- A statement with no qualifying row does not touch the state row: no update,
-- no row lock, dirty_since unchanged — as before, where no row fired the
-- update. A statement-level trigger also fires for 0 affected rows (and the
-- UPDATE one for every INSERT … ON CONFLICT DO UPDATE and MERGE with an
-- update action); the count is 0 there and nothing is written.
-- INSERT … ON CONFLICT DO UPDATE fires both triggers: inserted rows reach the
-- INSERT trigger's new_rows, updated rows the UPDATE trigger's old_rows and
-- new_rows, so their sum is the row-level count. COPY and MERGE fill the same
-- transition tables. now() is the transaction start, so dirty_since takes the
-- same value as before.
--
-- Lock timing. Row-level AFTER triggers fire at the end of the statement too,
-- queued per row in trigger-name order: the state row was locked by the first
-- qualifying row's turn, between the foreign-key checks (RI_ConstraintTrigger_*
-- sorts before trg_*) of the first row and those of the later rows. A
-- statement-level AFTER trigger fires after every row-level AFTER trigger of
-- its statement, so the state row is now locked after the foreign-key locks of
-- all rows and still held to commit. A multi-row write no longer holds the
-- state row while it waits for the parent or source row of a later row. The
-- order is now "foreign-key locks of EVERY row of the statement, then the state
-- row" — the order a one-row write already had, for all rows. Its other side:
-- while a multi-row write waits for the state row, it holds FOR KEY SHARE on
-- the parent and source rows of all its rows, not only of the first. A
-- transaction that holds the state row (after a counted block write) and then
-- takes FOR UPDATE, DELETE or a key update on such a parent or source row
-- closes a deadlock with it (40P01); before 177 only with the first row's. No
-- write path does that: every FOR UPDATE and DELETE on context_blocks runs
-- before its transaction's first counted write (UpsertBlock's pre-read, the
-- supersedes lock passes, the guard resolve batch, the issue update, the
-- scope purge) or in its own transaction (tenant prune batches), the picks
-- are SKIP LOCKED, and nothing updates a block id or a context_sources row. A
-- new write path keeps that rule: row locks first, block writes after. The
-- block-count triggers of 170 (trg_block_counts_ins/_upd) fire before
-- trg_guard_dirty by name and take no shared row lock.
--
-- Cost of the new triggers: the transition tables of INSERT and UPDATE already
-- exist for trg_block_counts_ins/_upd (one capture per statement, shared by
-- every trigger of the event); this adds a count over new_rows (INSERT) or an
-- anti-join of new_rows against old_rows on id (UPDATE), and one state-row
-- update per statement. Measured before/after: docs/operations.md,
-- "Migration 177".
--
-- The function carries SET enable_nestloop = off and SET jit = off. Its
-- queries have no parameters, so PL/pgSQL plans each of them once per backend
-- and keeps that generic plan, sized by the transition tables of the FIRST
-- call — join method and JIT decision included:
--   - A backend whose first update touched one row kept a nested loop over
--     old_rows, and a later update of n rows on that backend compared n x n
--     rows (testcontainer: 17.6 s in the trigger for 10 000 rows after a
--     one-row update, 15.9 ms on a fresh backend). Without the nested loop the
--     join is a hash or merge join whatever the first call held (8.1 ms for
--     the same 10 000 rows after a one-row update).
--   - A first call large enough to pass jit_above_cost (default 100 000,
--     reached by an update of roughly 1.5 million rows, e.g. a data migration
--     over the whole table on the application pool) kept a JIT-compiled plan,
--     and every later statement on that backend compiled again (production
--     image pgvector-timescaledb:pg18 with JIT, first call forced into JIT:
--     5 158 µs in trg_guard_dirty_upd per one-row update against 72 µs, 1 453
--     µs in trg_guard_dirty per one-row insert against 52 µs). With jit = off
--     on the function: 65 and 66 µs. The count queries are too small for JIT
--     to pay at any size.
--
-- LOCKS. SET LOCAL lock_timeout = '3s' (house pattern 145/166/170): a long
-- writer fails the boot instead of queueing every reader behind the DDL; the
-- next start retries. CREATE OR REPLACE TRIGGER and CREATE TRIGGER take SHARE
-- ROW EXCLUSIVE on context_blocks: they wait for in-flight writers, keep new
-- writers out until this migration commits and let readers pass. SHARE ROW
-- EXCLUSIVE also waits for every holder of SHARE UPDATE EXCLUSIVE on the
-- table: a manual VACUUM or ANALYZE, a CREATE INDEX CONCURRENTLY (the
-- re-embed arm builds its pick indexes that way), an anti-wraparound autovacuum
-- (a regular autovacuum cancels itself). Such a holder also fails the boot
-- with 55P03 after 3 s; start again once it has finished. DROP TRIGGER
-- would take ACCESS EXCLUSIVE and block readers, so the row-level trigger is
-- replaced in place under its name (trg_guard_dirty is the INSERT trigger now)
-- and the UPDATE trigger is created beside it. The function is replaced in the
-- same transaction (store.RunMigrations runs a file in one), so no writer sees
-- the new body with the old trigger or the other way round. Catalog changes
-- only: no row is read or written, the duration does not grow with the table.
--
-- Idempotent: CREATE OR REPLACE throughout.
--
-- Way back (an older binary does not name the function or the triggers), in
-- ONE transaction so no write falls between the two trigger shapes; DROP
-- TRIGGER takes ACCESS EXCLUSIVE:
--   BEGIN;
--   SET LOCAL lock_timeout = '3s';
--   DROP TRIGGER trg_guard_dirty_upd ON context_blocks;
--   CREATE OR REPLACE FUNCTION mark_guard_dirty() … (the definition of
--       113_baseline.sql; without SET clauses it drops enable_nestloop and
--       jit);
--   COMMENT ON FUNCTION mark_guard_dirty() IS NULL;  (it had none before)
--   CREATE OR REPLACE TRIGGER trg_guard_dirty AFTER INSERT OR UPDATE ON
--       context_blocks FOR EACH ROW EXECUTE FUNCTION mark_guard_dirty();
--   DELETE FROM _migrations WHERE version = 177;
--   COMMIT;
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE OR REPLACE FUNCTION mark_guard_dirty() RETURNS trigger
LANGUAGE plpgsql
SET enable_nestloop = off
SET jit = off
AS $$
DECLARE
    n bigint;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT count(*) INTO n
          FROM new_rows w
         WHERE w.category IS DISTINCT FROM 'index';
    ELSE
        SELECT count(*) INTO n
          FROM new_rows w
         WHERE w.category IS DISTINCT FROM 'index'
           AND NOT EXISTS (SELECT 1
                             FROM old_rows o
                            WHERE o.id = w.id
                              AND o.metadata ? 'guard_status'
                              AND w.metadata ? 'guard_status'
                              AND o.content = w.content);
    END IF;
    IF n > 0 THEN
        UPDATE context_guard_state
           SET dirty_since = COALESCE(dirty_since, now()),
               pending_count = pending_count + n;
    END IF;
    RETURN NULL;
END;
$$;

COMMENT ON FUNCTION mark_guard_dirty() IS
    'trg_guard_dirty / trg_guard_dirty_upd (migration 177): adds the statement''s blocks the guard has to look at to context_guard_state.pending_count in one update; skips category index and updates of a checked block (guard_status in old and new metadata) with unchanged content.';

CREATE OR REPLACE TRIGGER trg_guard_dirty
    AFTER INSERT ON context_blocks
    REFERENCING NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION mark_guard_dirty();

CREATE OR REPLACE TRIGGER trg_guard_dirty_upd
    AFTER UPDATE ON context_blocks
    REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION mark_guard_dirty();
