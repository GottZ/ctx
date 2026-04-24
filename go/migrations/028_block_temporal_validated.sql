-- Marker for "Temporal-Validation has run for this block at this version".
-- Phase 1 (deterministic pattern matching) writes to context_temporal cheaply.
-- Phase 2 (LLM review) is costly; we only want it once per block-content, not
-- on every cooldown recheck. This column records when the combined validation
-- most recently completed — if it's newer than updated_at, we skip.

ALTER TABLE context_blocks
    ADD COLUMN dream_temporal_validated_at TIMESTAMPTZ;

-- Helps the Dream picker filter blocks that need validation vs already done.
CREATE INDEX IF NOT EXISTS idx_blocks_dream_temporal_pending
    ON context_blocks (id)
    WHERE dream_temporal_validated_at IS NULL
      AND NOT is_archived;
