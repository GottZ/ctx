-- =============================================================================
-- 189_temporal_review_version.sql — the dream's temporal review is due by the
-- version of the block it reviewed, not by a time comparison (issue #452)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- The dream reviewed a block when updated_at was later than
-- dream_temporal_validated_at (migration 028, folded into 113_baseline.sql).
-- updated_at is now(), the start of the writing transaction, not the order
-- the writes commit in: a write that began before the stamp and committed
-- after it carried an earlier updated_at and read as reviewed (two windows,
-- issue #452 point 1). The review reads the block's title and content (and
-- its creation date, which no write changes); a write of tags or metadata
-- bumped updated_at and cost a review of what it had read already.
--
-- context_temporal_reviews holds, per block, the version the review's rows in
-- context_temporal stand for: version = sha256(content_hash || title) of the
-- title and content the review read (content_hash is the fixed-length hex
-- digest of the content, so the concatenation is unambiguous). The review
-- writes it in the transaction that writes its rows, under the same guard
-- (store.PopulateTemporalReview: only while the block holds that title and
-- content); every other writer of a block's temporal rows (the write
-- afterwork, the review's deterministic phase 1, the startup backfill)
-- deletes it in its transaction. A block is due when no row here carries the
-- version of its title and content (store.TemporalReviewDueCol, read by the
-- dream's pick for the one block it hands out). No comparison of times is
-- left, and the order in which writes commit does not matter.
--
-- A table, not a column of context_blocks: the row is written and deleted
-- with the temporal rows, which take no lock on the block row beyond the
-- FOR KEY SHARE of their foreign key (issue #421). A column would make every
-- write afterwork of a reviewed block write a row version of context_blocks
-- and hold the block row, which issue #421 took out of the afterwork.
--
-- dream_temporal_validated_at stays and is no longer written or read by the
-- binary of this migration; an older binary (the rollback image) reads and
-- writes it as before. idx_blocks_dream_temporal_pending (id) WHERE
-- dream_temporal_validated_at IS NULL AND NOT is_archived is dropped: no
-- statement reads it (it has had no reader since migration 028 shipped), and
-- with the column no longer written it would hold every block written from
-- now on. An older binary does not need it either.
--
-- STAMPS OF BEFORE (the inherit step). Every block is due when this table is
-- empty, which is one LLM review per block (live 2026-11: 11 274 blocks,
-- mean 3.9 s and 1.1k prompt tokens per review). Most stamps of the old
-- column are good. A stamp is bad when a title or content write committed
-- during the review it closed, or when the review failed and the older
-- binary stamped all the same (an LLM failure, an unparseable answer, a
-- database error). A write during the review carries an updated_at inside
-- the review's cycle, so such a stamp lies after the block's updated_at by
-- less than one cycle (before issue #427 the stamp was now() at its end;
-- since, the row's updated_at, which a later write undercuts by its own
-- transaction's length only). A failed LLM call or answer is in the LLM log:
-- the older binary records each dream-temporal call with block_ids and,
-- when the call or its parse failed, error. This file therefore takes over a
-- stamp as the review of the block's current title and content when
--   * the stamp equals updated_at (the stamp of #427, set while the block
--     held what was reviewed), or lies more than inherit_after (15 minutes)
--     after it: longer than the dream's default whole-cycle deadline (700 s)
--     plus the claim's grace (60 s), so no write during the review can be
--     the block's last; and
--   * the block's last dream-temporal call in context_llm_log did not fail
--     (or the log holds none for it).
-- It leaves due a block whose stamp lies after updated_at by up to 15
-- minutes, whose last logged review failed, whose updated_at is later than
-- its stamp (due under the old rule as well) or that has no stamp. Not seen:
-- a database error of the older review (phase 1 or the write of its findings
-- fails after or without a logged call that succeeded), and a log row the
-- older binary lost (its writer drops rows under pressure); such a stamp is
-- taken over. Live 2026-11-21 (read only): 1 776 stamps qualify by time, 15
-- of them closed a review whose last call failed. An instance whose
-- dream.cycle_timeout or timeouts.dream lets a review run past 14 minutes
-- can take back what this file took over with a bound of its own
-- (docs/operations.md, "Migration 189"); an inherited bad stamp costs the
-- block the LLM's extra dates until its title or content changes, as before.
--
-- SIZE AND LOCKS. The size decision is a bounded count (at most
-- inline_rows + 1 rows), not pg_class.reltuples, which lags behind the
-- table between ANALYZE runs (issue #399). The failed reviews are read from
-- context_llm_log into a temporary table before any lock is taken. Then the
-- file takes SHARE ROW EXCLUSIVE on context_blocks (readers go on, writers
-- wait) — the lock the foreign key's triggers need anyway; the runner runs
-- each file in ONE transaction (store/migrations.go), so it holds to the
-- commit and no write can commit between the inherit step's read and the
-- file's commit. The inherit step runs before the foreign key exists: an
-- insert under a foreign key locks its block row (FOR KEY SHARE), which
-- writes a lock record and, on the first touch of a heap page after a
-- checkpoint, a full-page image (with the foreign key in place, 199 MB of
-- WAL at 100 000 rows, 90 % of it those images). ADD CONSTRAINT then
-- validates all rows in one anti-join without row locks. DROP INDEX comes
-- last: it takes ACCESS EXCLUSIVE on context_blocks for a catalog change, so
-- readers wait for the commit only, not for the inherit step. `SET LOCAL
-- lock_timeout = '3s'` (house pattern) bounds the wait for the locks: a
-- transaction that holds a block row longer (a guard batch, a write of a
-- second ctxd instance) fails the file with 55P03, it rolls back with
-- nothing changed, and the next boot tries again. Measured at 100 000 rows
-- of the live width (66 667 taken over; docs/operations.md, "Migration
-- 189"): the file 0.97 s, a writer waits 0.98 s, a reader 15 ms, 15.7 MB of
-- WAL. The inherit step runs inline up to 100 000 rows, about a second of
-- writers waiting, and is left to the operator above (WARNING, RUNBOOK);
-- until then every block is due at its next pick.
--
-- RUNBOOK above the threshold (WARNING "context_temporal_reviews: ..."): with
-- the dream off from the upgrade until the RUNBOOK is done
-- (CTX_DREAM_ENABLED=false for the start, or the dream mode off), so no
-- review runs and no temporal writer deletes a version without a write that
-- moves updated_at past the stamp — a repeat of a batch is then harmless.
-- Once, the failed reviews:
--
--     CREATE TEMP TABLE m189_failed_review AS
--     SELECT block_id FROM (
--         SELECT DISTINCT ON (u.block_id) u.block_id, l.error IS NOT NULL AS failed
--           FROM context_llm_log l CROSS JOIN LATERAL unnest(l.block_ids) AS u(block_id)
--          WHERE l.pipeline = 'dream-temporal'
--          ORDER BY u.block_id, l.created_at DESC) last
--      WHERE failed;
--
-- then one transaction per batch of ids (about 50 000 ids per batch;
-- readers go on, writers wait for the batch):
--
--     BEGIN;
--     SET LOCAL lock_timeout = '3s';
--     LOCK TABLE context_blocks IN SHARE ROW EXCLUSIVE MODE;
--     INSERT INTO context_temporal_reviews (block_id, version)
--     SELECT b.id, digest(b.content_hash || b.title, 'sha256')
--       FROM context_blocks b
--      WHERE b.id >= $lo AND b.id < $hi
--        AND b.dream_temporal_validated_at IS NOT NULL
--        AND (b.dream_temporal_validated_at = b.updated_at
--             OR b.dream_temporal_validated_at > b.updated_at + interval '15 minutes')
--        AND NOT EXISTS (SELECT 1 FROM m189_failed_review f WHERE f.block_id = b.id)
--     ON CONFLICT (block_id) DO NOTHING;
--     COMMIT;
--
-- A block changed since the upgrade keeps what the binary wrote (DO
-- NOTHING; the change moved updated_at past the stamp). The table lock keeps
-- a write from committing between the batch's read of updated_at and its
-- commit: a same-content write there could run its temporal afterwork (which
-- deletes the block's row here) before the batch commits, and the batch
-- would record a review over the afterwork's rows. With the dream running,
-- a review that fails after its phase 1 deleted the version, on a block no
-- write moved, would be taken over by a later batch.
--
-- WAY BACK (forward-only tree): an older binary ignores the table and needs
-- neither the table nor the index; its schema contract reports schema 189
-- as breaking (no embedded file for _migrations 189, the missing index), so
-- it starts in contract mode warn (the default) and refuses to start in mode
-- enforce until CTX_CONTRACT_MODE=warn. Its temporal writers do not delete a
-- block's row here: stop every older instance before this binary starts, and
-- after a rollback, stop the older instances, TRUNCATE
-- context_temporal_reviews, then start this binary (and run the RUNBOOK as
-- above). To restore the catalog of migration 188, out of band:
--     SET lock_timeout = '3s';
--     DROP TABLE context_temporal_reviews;
--     CREATE INDEX CONCURRENTLY idx_blocks_dream_temporal_pending ON context_blocks (id)
--         WHERE dream_temporal_validated_at IS NULL AND NOT is_archived;
--     DELETE FROM _migrations WHERE version = 189;
--
-- One table is added, no column of context_blocks: test.sh T07 counts 71
-- tables, 45 columns. Idempotent, forward-only. Registration by the runner
-- (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

DO $do$
DECLARE
    inherit_after CONSTANT INTERVAL := interval '15 minutes';
    inline_rows   CONSTANT BIGINT   := 100000;
    v_rows        BIGINT;
    v_inherited   BIGINT;
BEGIN
    SELECT count(*) INTO v_rows FROM (SELECT 1 FROM context_blocks LIMIT inline_rows + 1) s;

    IF v_rows <= inline_rows THEN
        -- Before the lock (SIZE AND LOCKS): the blocks whose last temporal
        -- review failed, by the LLM log.
        CREATE TEMP TABLE m189_failed_review ON COMMIT DROP AS
        SELECT block_id FROM (
            SELECT DISTINCT ON (u.block_id) u.block_id, l.error IS NOT NULL AS failed
              FROM context_llm_log l
             CROSS JOIN LATERAL unnest(l.block_ids) AS u(block_id)
             WHERE l.pipeline = 'dream-temporal'
             ORDER BY u.block_id, l.created_at DESC) last
         WHERE failed;
    END IF;

    LOCK TABLE context_blocks IN SHARE ROW EXCLUSIVE MODE;

    CREATE TABLE IF NOT EXISTS context_temporal_reviews (
        block_id UUID  PRIMARY KEY,
        version  BYTEA NOT NULL
    );

    IF v_rows > inline_rows THEN
        RAISE WARNING 'context_temporal_reviews: more than % rows in context_blocks, the stamps of dream_temporal_validated_at were not taken over and every block is due for a temporal review at its next pick; run the RUNBOOK of migration 189 (docs/operations.md, "Migration 189")',
            inline_rows;
    ELSE
        INSERT INTO context_temporal_reviews (block_id, version)
        SELECT b.id, digest(b.content_hash || b.title, 'sha256')
          FROM context_blocks b
         WHERE b.dream_temporal_validated_at IS NOT NULL
           AND (b.dream_temporal_validated_at = b.updated_at
                OR b.dream_temporal_validated_at > b.updated_at + inherit_after)
           AND NOT EXISTS (SELECT 1 FROM m189_failed_review f WHERE f.block_id = b.id)
        ON CONFLICT (block_id) DO NOTHING;
        GET DIAGNOSTICS v_inherited = ROW_COUNT;
        RAISE NOTICE 'context_temporal_reviews: % stamp(s) of dream_temporal_validated_at taken over as reviews of the blocks'' current title and content', v_inherited;
    END IF;

    -- The foreign key after the inherit step (SIZE AND LOCKS), under the name
    -- PostgreSQL gives REFERENCES in CREATE TABLE.
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'context_temporal_reviews_block_id_fkey'
                      AND conrelid = 'public.context_temporal_reviews'::regclass) THEN
        ALTER TABLE context_temporal_reviews
            ADD CONSTRAINT context_temporal_reviews_block_id_fkey
            FOREIGN KEY (block_id) REFERENCES context_blocks(id) ON DELETE CASCADE;
    END IF;
END
$do$;

COMMENT ON TABLE context_temporal_reviews IS
    'The version (sha256 of content_hash || title) the dream''s temporal review rows of a block stand for; written with them, deleted by every other temporal writer (migration 189, issue #452).';

-- Last: ACCESS EXCLUSIVE from here to the commit (SIZE AND LOCKS).
DROP INDEX IF EXISTS idx_blocks_dream_temporal_pending;
