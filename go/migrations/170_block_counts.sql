-- =============================================================================
-- 170_block_counts.sql — block counts per scope are kept with the writes and
-- read instead of counted (issue #266)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- manage stats, guard-stats, list-categories and the digest envelope counted
-- their numbers from context_blocks on every call (count(*), count(DISTINCT
-- category), GROUP BY category over the caller's read scopes) — linear in the
-- corpus, and the statusline asks on every prompt. context_block_counts holds
-- one row per (scope, category, is_archived, guard_status) with the number of
-- blocks carrying exactly that key; a stats call sums the rows of its read
-- scopes. Every number those surfaces show is a sum over this key with the
-- predicates the old queries had:
--   total_blocks / guard total   NOT is_archived
--   total_categories             categories with a positive NOT is_archived sum
--   list-categories              NOT is_archived, summed per category
--   guard active/clean/…         NOT is_archived AND guard_status = '<value>'
--   guard unchecked              NOT is_archived AND guard_status IS NULL
--   guard archived_dups          guard_status = 'archived_dup' (archived or not)
-- guard_status is nullable in context_blocks and NULL is a counted state of its
-- own (unchecked), so the key is UNIQUE NULLS NOT DISTINCT: one NULL row per
-- (scope, category, is_archived), and ON CONFLICT finds it.
--
-- Maintenance: three statement-level AFTER triggers with transition tables
-- (INSERT / UPDATE / DELETE; PostgreSQL allows one event per trigger when
-- transition tables are used, and no column list on the UPDATE one). Each
-- statement aggregates its rows into deltas per key and applies them in ONE
-- upsert, in key order: a bulk insert or a purge batch of 10 000 rows touches
-- a handful of counter rows, and two statements always lock shared counter
-- rows in the same order. An UPDATE that changes no key (embedding, metadata,
-- lifecycle) nets to zero per key and writes nothing (HAVING sum <> 0). A
-- trigger covers every writer — store functions, migrations, manual SQL, the
-- FK cascade of fk_blocks_parent — where code in the store would not. What it
-- does not see: TRUNCATE (own trigger below), ALTER TABLE … DISABLE TRIGGER
-- and session_replication_role = replica (pg_restore --disable-triggers);
-- after those, `ctx manage stats-reconcile` (or the scheduler's reconcile
-- loop, scheduler.block_count_reconcile_interval) brings the counters back.
--
-- A counter row that reaches 0 stays (a concurrent increment of the same key
-- needs no re-insert race); the reconcile removes zero rows it finds.
--
-- Backfill exactness. The file runs in one transaction (store.RunMigrations).
-- LOCK … SHARE ROW EXCLUSIVE comes first: it waits for in-flight writers of
-- context_blocks (ROW EXCLUSIVE) to commit and keeps new ones out until this
-- migration commits; readers (ACCESS SHARE) are not blocked. Under that lock
-- the triggers are created and the counts are taken by one INSERT … SELECT
-- whose READ COMMITTED snapshot starts after the lock was granted, so it sees
-- every committed write, and no write can fall between the count and the
-- trigger: every write after the commit goes through the trigger. Measured
-- (testcontainer, 1 000 000 blocks): see docs/operations.md "Block counters".
-- lock_timeout 3s (house pattern 145/166) fails the boot instead of queueing
-- behind a long writer; the next start retries. ctxd applies migrations before
-- it serves, so during the backfill this process takes no requests; another
-- ctx process on the same database waits with its writes.
--
-- Idempotent: the table is created if missing and its rows are recomputed
-- under the lock; the triggers are dropped and created again.
--
-- Way back: DROP TRIGGER trg_block_counts_ins, trg_block_counts_upd,
-- trg_block_counts_del, trg_block_counts_trunc ON context_blocks; DROP
-- FUNCTION context_block_counts_ins(), context_block_counts_upd(),
-- context_block_counts_del(), context_block_counts_trunc(); DROP TABLE
-- context_block_counts; DELETE FROM _migrations WHERE version = 170. An older
-- binary does not read the table.
--
-- New table: the test.sh T07 fallback count moves 66 → 67 tables, the
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
    'Blocks per (scope, category, is_archived, guard_status), maintained by the trg_block_counts_* triggers on context_blocks (migration 170). Read by store.GetStats, GetGuardStats, ListCategories, ActiveCategoryCount.';

CREATE OR REPLACE FUNCTION context_block_counts_ins() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO context_block_counts AS c (scope, category, is_archived, guard_status, n)
    SELECT r.scope, r.category, r.is_archived, r.guard_status, count(*)
      FROM new_rows r
     GROUP BY 1, 2, 3, 4
     ORDER BY 1, 2, 3, 4
    ON CONFLICT (scope, category, is_archived, guard_status)
    DO UPDATE SET n = c.n + EXCLUDED.n;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION context_block_counts_del() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO context_block_counts AS c (scope, category, is_archived, guard_status, n)
    SELECT r.scope, r.category, r.is_archived, r.guard_status, -count(*)
      FROM old_rows r
     GROUP BY 1, 2, 3, 4
     ORDER BY 1, 2, 3, 4
    ON CONFLICT (scope, category, is_archived, guard_status)
    DO UPDATE SET n = c.n + EXCLUDED.n;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION context_block_counts_upd() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO context_block_counts AS c (scope, category, is_archived, guard_status, n)
    SELECT d.scope, d.category, d.is_archived, d.guard_status, sum(d.dn)
      FROM (SELECT o.scope, o.category, o.is_archived, o.guard_status, -1 AS dn FROM old_rows o
            UNION ALL
            SELECT w.scope, w.category, w.is_archived, w.guard_status, 1 FROM new_rows w) d
     GROUP BY 1, 2, 3, 4
    HAVING sum(d.dn) <> 0
     ORDER BY 1, 2, 3, 4
    ON CONFLICT (scope, category, is_archived, guard_status)
    DO UPDATE SET n = c.n + EXCLUDED.n;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION context_block_counts_trunc() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM context_block_counts;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS trg_block_counts_ins ON context_blocks;
CREATE TRIGGER trg_block_counts_ins
    AFTER INSERT ON context_blocks
    REFERENCING NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION context_block_counts_ins();

DROP TRIGGER IF EXISTS trg_block_counts_upd ON context_blocks;
CREATE TRIGGER trg_block_counts_upd
    AFTER UPDATE ON context_blocks
    REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
    FOR EACH STATEMENT EXECUTE FUNCTION context_block_counts_upd();

DROP TRIGGER IF EXISTS trg_block_counts_del ON context_blocks;
CREATE TRIGGER trg_block_counts_del
    AFTER DELETE ON context_blocks
    REFERENCING OLD TABLE AS old_rows
    FOR EACH STATEMENT EXECUTE FUNCTION context_block_counts_del();

DROP TRIGGER IF EXISTS trg_block_counts_trunc ON context_blocks;
CREATE TRIGGER trg_block_counts_trunc
    AFTER TRUNCATE ON context_blocks
    FOR EACH STATEMENT EXECUTE FUNCTION context_block_counts_trunc();

DELETE FROM context_block_counts;

INSERT INTO context_block_counts (scope, category, is_archived, guard_status, n)
SELECT scope, category, is_archived, guard_status, count(*)
  FROM context_blocks
 GROUP BY 1, 2, 3, 4;
