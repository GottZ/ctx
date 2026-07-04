-- =============================================================================
-- 086_workflow_created_index.sql — immutable-keyset board index for ?sort=created
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- Workflow-engine axis 03, Welle W6 (design/03-workflow-api-cli.md §6.1).
-- =============================================================================
-- The board index idx_blocks_workflow_board (M077) orders on updated_at DESC —
-- the UI board view. updated_at is MUTABLE: a row updated mid-pagination moves
-- ahead of the consumed cursor and drops out of the traversal (documented list
-- semantics, §6.1). For the LOSSLESS traversal that agents/export need, W6 offers
-- ?sort=created — keyset on the IMMUTABLE (created_at, id). That ordering needs
-- its own index: the board index cannot serve a created_at-ordered range scan
-- (created_at is not one of its keys), so an ORDER BY created_at over the scope
-- would force a Sort node (the exact RED the board index avoids for updated_at).
--
-- K4 NOTE (design/03 §6.1, masterplan K4): the q-filter was decided to bind to
-- the EXISTING FTS tsvector GIN path (idx_context_ts_de / idx_context_ts_en,
-- M001/M044) — no trigram migration is needed (idx_trgm_title already exists too,
-- M001). This freed the W6 migration slot (086) for the created-sort index below.
--
-- Ordering + partiality mirror the board index (M077) exactly so the same keyset
-- machinery (tuple comparison against the all-DESC index direction) applies:
--   - all-DESC (created_at DESC, id DESC) ⇒ (created_at, id) < (cur) is a clean
--     ordered range scan, no Sort, no bitmap.
--   - workflow_status is NOT an index key (unlike the board index): created_at is
--     monotone across ALL statuses, so ONE range scan serves both the
--     status-filtered AND the status-unfiltered created traversal WITHOUT a
--     per-status merge — the architectural simplification created-sort buys over
--     the updated board path (§6.1).
--   - partial WHERE workflow_status IS NOT NULL AND NOT is_archived keeps it lean
--     (only live workflow rows), same as M077; a created-sort query MUST carry the
--     `workflow_status IS NOT NULL` predicate to match this partial index.
--
-- INDEX NOTE (063/069/077 house norm): plain CREATE INDEX in the single-Tx runner
-- (CONCURRENTLY forbidden) holds a SHARE lock for the build. Fine at today's
-- scale; at 1M+ rows build it OUT-OF-BAND first with
--   CREATE INDEX CONCURRENTLY idx_blocks_workflow_created ...
-- and the IF NOT EXISTS below then finds it (this migration becomes a no-op).
--
-- Backfill: none — additive index, no data change (pausability invariant).
-- =============================================================================

SET LOCAL lock_timeout = '3s';

CREATE INDEX IF NOT EXISTS idx_blocks_workflow_created
    ON context_blocks (scope, type_name, created_at DESC, id DESC)
    WHERE workflow_status IS NOT NULL AND NOT is_archived;

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (86, '086_workflow_created_index.sql', now())
  ON CONFLICT (version) DO NOTHING;
