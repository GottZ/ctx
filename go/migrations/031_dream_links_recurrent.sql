-- =============================================================================
-- 031_dream_links_recurrent.sql — Recurrent relationship class (Welle 38b)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Welle 38b adds a fifth Dream-Link relationship: 'recurrent'.
--
-- Existing classes (topical/factual/causal/supersedes) cover semantic similarity,
-- knowledge-implication, decision-cause, and version-replacement. They miss the
-- recurring-instance case: blocks that share a temporal-pattern AND title-shape
-- but are valid in parallel (e.g. mautrix-{signal,whatsapp,discord} bridges,
-- Session-N handovers, sequential project-phase snapshots, periodised CV ranges).
--
-- Pre-Empirie 2026-05-06 (audit-w38b-results.json): 27 Phase-1 candidates
-- (sim>0.5, same dim+value in context_temporal) → 18 RECURRENT + 2 SUPERSEDES
-- + 7 NEITHER per Sub-Agent-Audit. 74% Phase-1 precision.
--
-- Detection in dream pipeline (recurrence.go):
--   Phase 1: SQL pre-filter (same dim+value + title-sim > 0.5)
--   Phase 2: LLM-confirm with explicit pattern classification
--            (weekly/monthly/sessional/parallel/sequence/version-replacement)
--
-- raw_confidence floor for 'recurrent' is 0.8 (linkfilters.go) — higher than
-- the 0.7 floor for other types because Phase 2 LLM-classification has more
-- error-modes than the existing topical/factual/causal evaluation.
-- =============================================================================

ALTER TABLE context_dream_links
  DROP CONSTRAINT IF EXISTS context_dream_links_relationship_check;

ALTER TABLE context_dream_links
  ADD CONSTRAINT context_dream_links_relationship_check
  CHECK (relationship = ANY (ARRAY[
    'topical'::text,
    'factual'::text,
    'causal'::text,
    'supersedes'::text,
    'recurrent'::text
  ]));

-- Partial index for recurrent-only queries (e.g. recurrent-cluster lookups,
-- supersedes-promotion-candidate scans).
CREATE INDEX IF NOT EXISTS idx_dream_links_recurrent
  ON context_dream_links (source_block_id, target_block_id)
  WHERE relationship = 'recurrent';

INSERT INTO _migrations (version, filename, applied_at)
  VALUES (31, '031_dream_links_recurrent.sql', now())
  ON CONFLICT (version) DO NOTHING;
