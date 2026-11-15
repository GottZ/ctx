-- =============================================================================
-- 170_block_counts.sql — block counts per scope are kept with the writes and
-- read instead of counted (issue #266)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- manage stats, guard-stats, list-categories and the digest envelope counted
-- their numbers from context_blocks on every call (count(*), count(DISTINCT
-- category), GROUP BY category over the caller's read scopes) — linear in the
-- corpus, and the statusline asks on every prompt. Since this migration they
-- sum counter rows instead. Every number those surfaces show is a sum over the
-- key (scope, category, is_archived, guard_status) with the predicates the old
-- queries had:
--   total_blocks / guard total   NOT is_archived
--   total_categories             categories with a positive NOT is_archived sum
--   list-categories              NOT is_archived, summed per category
--   guard active/clean/…         NOT is_archived AND guard_status = '<value>'
--   guard unchecked              NOT is_archived AND guard_status IS NULL
--   guard archived_dups          guard_status = 'archived_dup' (archived or not)
-- guard_status is nullable in context_blocks and NULL is a counted state of its
-- own (unchecked); GROUP BY and UNIQUE NULLS NOT DISTINCT treat it as one key.
--
-- Two tables. context_block_count_deltas is append-only: three statement-level
-- AFTER triggers with transition tables (INSERT / UPDATE / DELETE; PostgreSQL
-- allows one event per trigger with transition tables, and no column list on
-- the UPDATE one) aggregate each statement's rows into one delta row per key
-- and INSERT them. A writer never updates or locks a shared row: no hot row,
-- no lock wait, no new lock-order edge between writers — a counter row taken
-- at the end of a statement and held to the commit could close a deadlock
-- cycle with the block rows a statement locks first (PR #269, finding 1). An
-- UPDATE that changes no key (embedding, metadata, lifecycle) nets to zero per
-- key and inserts nothing (HAVING sum <> 0). context_block_counts holds the
-- folded totals, one row per key (UNIQUE NULLS NOT DISTINCT); the scheduler
-- folds the delta rows into it every few seconds (store.FoldBlockCountDeltas:
-- one transaction deletes the delta rows its snapshot sees and adds their sum,
-- so a reader's snapshot sees either side with the same total). A reader sums
-- both tables for its read scopes: the counter rows of those scopes plus the
-- delta rows written since the last fold.
--
-- A trigger covers every writer — store functions, migrations, manual SQL, the
-- FK cascade of fk_blocks_parent — where code in the store would not. What it
-- does not see: TRUNCATE (own trigger below, clears both tables), ALTER TABLE …
-- DISABLE TRIGGER and session_replication_role = replica (pg_restore
-- --disable-triggers); after those, `ctx manage stats-reconcile` (or the
-- scheduler's pass, scheduler.block_count_reconcile_interval) brings the
-- counters back. A counter row that folds to 0 stays; the reconcile removes it.
--
-- Backfill exactness. The file runs in one transaction (store.RunMigrations).
-- LOCK … SHARE ROW EXCLUSIVE comes first: it waits for in-flight writers of
-- context_blocks (ROW EXCLUSIVE) to commit and keeps new ones out until this
-- migration commits; readers (ACCESS SHARE) are not blocked. Under that lock
-- the triggers are created and the counts are taken by one INSERT … SELECT
-- whose READ COMMITTED snapshot starts after the lock was granted, so it sees
-- every committed write, and no write can fall between the count and the
-- trigger: every write after the commit goes through the trigger. Measured
-- (testcontainer, 1 000 000 blocks): docs/operations.md, "Migration 170".
-- lock_timeout 3s (house pattern 145/166) fails the boot instead of queueing
-- behind a long writer; the next start retries. ctxd applies migrations before
-- it serves, so during the backfill this process takes no requests; another
-- ctx process on the same database waits with its writes.
--
-- Idempotent: the tables are created if missing and their rows are recomputed
-- under the lock; functions and triggers are replaced (CREATE OR REPLACE
-- TRIGGER takes the SHARE ROW EXCLUSIVE this file holds; DROP TRIGGER of an
-- existing trigger would take ACCESS EXCLUSIVE and block readers).
--
-- Way back: DROP TRIGGER trg_block_counts_ins, trg_block_counts_upd,
-- trg_block_counts_del, trg_block_counts_trunc ON context_blocks; DROP
-- FUNCTION context_block_counts_ins(), context_block_counts_upd(),
-- context_block_counts_del(), context_block_counts_trunc(); DROP TABLE
-- context_block_count_deltas, context_block_counts; DELETE FROM _migrations
-- WHERE version = 170. An older binary does not read the tables.
--
-- New tables: the test.sh T07 fallback count moves 66 → 68 tables, the
-- context_blocks column count stays 42.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

LOCK TABLE context_blocks IN SHARE ROW EXCLUSIVE MODE;

CREATE TABLE IF NOT EXISTS context_block_counts (
    scope        TEXT    NOT NULL,
    category     TEXT    NOT NULL,
    is_archived  BOOLEAN NOT NULL,
    guard_status TEXT,
    n            BIGINT  NOT NULL,
    CONSTRAINT context_block_counts_key
        UNIQUE NULLS NOT DISTINCT (scope, category, is_archived, guard_status)
);

COMMENT ON TABLE context_block_counts IS
    'Folded block counts per (scope, category, is_archived, guard_status) (migration 170). A count is this row plus the context_block_count_deltas rows of the key.';

CREATE TABLE IF NOT EXISTS context_block_count_deltas (
    scope        TEXT    NOT NULL,
    category     TEXT    NOT NULL,
    is_archived  BOOLEAN NOT NULL,
    guard_status TEXT,
    n            BIGINT  NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_block_count_deltas_scope ON context_block_count_deltas (scope);

COMMENT ON TABLE context_block_count_deltas IS
    'Append-only block-count deltas, one row per key and statement, written by the trg_block_counts_* triggers on context_blocks and folded into context_block_counts by the scheduler (migration 170).';

CREATE OR REPLACE FUNCTION context_block_counts_ins() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO context_block_count_deltas (scope, category, is_archived, guard_status, n)
    SELECT r.scope, r.category, r.is_archived, r.guard_status, count(*)
      FROM new_rows r
     GROUP BY 1, 2, 3, 4;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION context_block_counts_del() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO context_block_count_deltas (scope, category, is_archived, guard_status, n)
    SELECT r.scope, r.category, r.is_archived, r.guard_status, -count(*)
      FROM old_rows r
     GROUP BY 1, 2, 3, 4;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION context_block_counts_upd() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO context_block_count_deltas (scope, category, is_archived, guard_status, n)
    SELECT x.scope, x.category, x.is_archived, x.guard_status, sum(x.dn)
      FROM (SELECT o.scope, o.category, o.is_archived, o.guard_status, -1 AS dn FROM old_rows o
            UNION ALL
            SELECT w.scope, w.category, w.is_archived, w.guard_status, 1 FROM new_rows w) x
     GROUP BY 1, 2, 3, 4
    HAVING sum(x.dn) <> 0;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION context_block_counts_trunc() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM context_block_count_deltas;
    DELETE FROM context_block_counts;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE TRIGGER trg_block_counts_ins
    AFTER INSERT ON context_blocks
    REFERENCING NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION context_block_counts_ins();

CREATE OR REPLACE TRIGGER trg_block_counts_upd
    AFTER UPDATE ON context_blocks
    REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION context_block_counts_upd();

CREATE OR REPLACE TRIGGER trg_block_counts_del
    AFTER DELETE ON context_blocks
    REFERENCING OLD TABLE AS old_rows
    FOR EACH STATEMENT EXECUTE FUNCTION context_block_counts_del();

CREATE OR REPLACE TRIGGER trg_block_counts_trunc
    AFTER TRUNCATE ON context_blocks
    FOR EACH STATEMENT EXECUTE FUNCTION context_block_counts_trunc();

DELETE FROM context_block_count_deltas;
DELETE FROM context_block_counts;

INSERT INTO context_block_counts (scope, category, is_archived, guard_status, n)
SELECT scope, category, is_archived, guard_status, count(*)
  FROM context_blocks
 GROUP BY 1, 2, 3, 4;
