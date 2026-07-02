-- =============================================================================
-- 075_drop_is_meta.sql — drop the materialised is_meta column + its index
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Workflow-engine axis 01, wave T9 (design/01-type-registry.md §3.7/§7-T9).
-- The third type axis is consolidated: is_meta (M029, empirical dream-noise
-- exclude) was live congruent with type_name='system-meta' (verified on the
-- production corpus 2026-07-02: 24/24 rows, 0 asymmetric of 1141), and its
-- only behavioural effect — dream exclusion on both the pick and the
-- candidate/target side — moved into the registry policy `dream.linkable`
-- (false on the system-meta seed) in wave T8. Since T8 no code READS the
-- column (the five eligibility mirrors + the candidate batch-lookup consume
-- the DreamLinkableTypes allowlist); since this wave the classify hook no
-- longer WRITES it either (store/classify.go UPDATE carries type_name only).
--
--   metadata.is_meta (the JSONB KEY) deliberately survives: it is a classify
--   INPUT (the system-meta seed's classify.metadata_flags rule fires on it).
--   Only the materialised column falls.
--
-- Steps:
--   1. DROP INDEX idx_blocks_is_meta (M029's partial picker index — carried
--      only the retired NOT-is_meta dream-eligibility scans; dropped BEFORE
--      the column so the statement order reads as intent, though DROP COLUMN
--      would cascade it anyway).
--   2. DROP COLUMN is_meta.
--
-- Both steps are metadata-only catalog ops (no table rewrite): a short
-- ACCESS EXCLUSIVE lock, trivially safe at 1M+ rows; the reclaimed bytes are
-- reused by future row versions (standard PG column drop semantics).
--
-- Historical migrations (029, 036, 044, …) keep referencing is_meta: on a
-- fresh DB they run at their own position, BEFORE this drop — forward-only
-- ordering guarantees validity (the M070/M071 rename line's precedent).
--
-- Idempotent (IF EXISTS on both drops, _migrations ON CONFLICT).
-- Forward-only, self-registering.
-- =============================================================================

SET LOCAL lock_timeout = '3s';

DROP INDEX IF EXISTS idx_blocks_is_meta;

ALTER TABLE context_blocks DROP COLUMN IF EXISTS is_meta;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (75, '075_drop_is_meta.sql', now())
  ON CONFLICT (version) DO NOTHING;
