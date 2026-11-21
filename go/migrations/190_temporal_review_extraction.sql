-- =============================================================================
-- 190_temporal_review_extraction.sql — a version row of the dream's temporal
-- review records the extraction its rows were written with (issue #466 point 8)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- context_temporal_reviews (migration 189) holds, per block, the version
-- (sha256 of content_hash || title) the review's rows in context_temporal
-- stand for. The afterwork of a write that stores the content a block holds
-- (a same-content upsert) writes nothing when those rows hold every date its
-- extraction yields (store.temporalReviewCoversSQL): a superset test. A date
-- a later release's ExtractDates no longer yields (issue #437 dropped the
-- years outside 2020-2099) therefore stayed on a reviewed block, and a
-- backfill through RefreshTemporal would skip it.
--
-- extraction is sha256 over the version and a digest of the times the
-- deterministic extraction yielded for the reviewed content (the block's
-- creation time and ExtractDates; store.temporalExtractionDigest), written by
-- the review with the version (store.PopulateTemporalReview). The same-content
-- afterwork takes the rows as covering only when the row carries the
-- extraction of the version it holds and of the extraction in force: a
-- changed extraction rewrites the rows from it and deletes the version, and
-- the block is reviewed again. Bound to the version, so a version that an
-- older binary's review changes (its ON CONFLICT sets version only) leaves
-- an extraction that matches no write of the new version; a review of the
-- same version by an older binary leaves the extraction in force.
--
-- NULL is "extraction unknown": the rows taken over by migration 189 and
-- those an older binary's review writes. Such a row covers no same-content
-- write — the afterwork rewrites the rows and deletes the version, one LLM
-- review of the block — and keeps the block reviewed for the pick, which
-- reads the version only (store.TemporalReviewDueCol). Live 2026-11-21: at
-- most 1 762 rows, each costing one review when a same-content write or a
-- RefreshTemporal backfill reaches its block (docs/operations.md, "Migration
-- 190").
--
-- SIZE AND LOCKS. ADD COLUMN of a nullable column without a default changes
-- the catalog only: no row is rewritten or read, at any size. It takes ACCESS
-- EXCLUSIVE on context_temporal_reviews from the ALTER to the commit (the
-- runner runs the file in one transaction); `SET LOCAL lock_timeout = '3s'`
-- (house pattern) bounds the wait for a temporal writer or a pick in flight,
-- and on 55P03 the file rolls back and the next boot retries. No backfill:
-- the extraction of a row of before is not known.
--
-- WAY BACK (forward-only tree): the binary of migration 189 (v5.72.0) does
-- not know the column; its review writes version rows without it (NULL on an
-- insert, the earlier value beside the new version on a conflict — bound to
-- the earlier version, so covering nothing). Its schema contract reports the
-- column as a param drift and _migrations 190 without an embedded file as
-- breaking: it starts in contract mode warn (the default) and refuses to
-- start in mode enforce until CTX_CONTRACT_MODE=warn. To restore the catalog
-- of migration 189, out of band:
--     SET lock_timeout = '3s';
--     ALTER TABLE context_temporal_reviews DROP COLUMN extraction;
--     DELETE FROM _migrations WHERE version = 190;
--
-- No table and no column of context_blocks: test.sh T07 still counts 71
-- tables, 45 columns. Idempotent, forward-only. Registration by the runner
-- (store.RunMigrations).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

ALTER TABLE context_temporal_reviews ADD COLUMN IF NOT EXISTS extraction BYTEA;

COMMENT ON COLUMN context_temporal_reviews.extraction IS
    'sha256 of the version and of the times the extraction yielded for the reviewed content (creation time and ExtractDates); NULL: unknown, covers no same-content write (migration 190, issue #466).';
